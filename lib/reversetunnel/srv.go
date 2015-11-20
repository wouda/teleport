/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package reversetunnel

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/codahale/lunk"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/log"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/roundtrip"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/trace"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/mailgun/oxy/forward"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
)

type RemoteSite interface {
	ConnectToServer(addr, user string, auth []ssh.AuthMethod) (*ssh.Client, error)
	DialServer(addr string) (net.Conn, error)
	GetLastConnected() time.Time
	GetName() string
	GetStatus() string
	GetClient() *auth.Client
}

type Server interface {
	GetSites() []RemoteSite
	GetSite(name string) (RemoteSite, error)
	FindSimilarSite(name string) (RemoteSite, error)
	Start() error
	Wait()
}

type server struct {
	sync.RWMutex

	ap          auth.AccessPoint
	certChecker ssh.CertChecker
	l           net.Listener
	srv         *sshutils.Server

	sites []*remoteSite
}

// New returns an unstarted server
func NewServer(addr utils.NetAddr, hostSigners []ssh.Signer,
	ap auth.AccessPoint) (Server, error) {
	srv := &server{
		sites: []*remoteSite{},
		ap:    ap,
	}
	s, err := sshutils.NewServer(
		addr,
		srv,
		hostSigners,
		sshutils.AuthMethods{
			PublicKey: srv.keyAuth,
		})
	if err != nil {
		return nil, err
	}
	srv.certChecker = ssh.CertChecker{IsAuthority: srv.isAuthority}
	srv.srv = s
	return srv, nil
}

func (s *server) Wait() {
	s.srv.Wait()
}

func (s *server) Addr() string {
	return s.srv.Addr()
}

func (s *server) Start() error {
	return s.srv.Start()
}

func (s *server) Close() error {
	return s.srv.Close()
}

func (s *server) HandleNewChan(sconn *ssh.ServerConn, nch ssh.NewChannel) {
	log.Infof("got new channel request: %v", nch.ChannelType())
	switch nch.ChannelType() {
	case chanHeartbeat:
		log.Infof("got heartbeat request from agent: %v", sconn)
		site, err := s.upsertSite(sconn)
		if err != nil {
			log.Errorf("failed to upsert site: %v", err)
			nch.Reject(ssh.ConnectionFailed, "failed to upsert site")
			return
		}
		ch, req, err := nch.Accept()
		if err != nil {
			log.Errorf("failed to accept channel: %v", err)
			sconn.Close()
			return
		}
		go site.handleHeartbeat(ch, req)
	}
}

// isAuthority is called during checking the client key, to see if the signing
// key is the real CA authority key.
func (s *server) isAuthority(auth ssh.PublicKey) bool {
	keys, err := s.getTrustedCAKeys()
	if err != nil {
		log.Errorf("failed to retrieve trused keys, err: %v", err)
		return false
	}
	for _, k := range keys {
		if sshutils.KeysEqual(k, auth) {
			return true
		}
	}
	return false
}

func (s *server) getTrustedCAKeys() ([]ssh.PublicKey, error) {
	out := []ssh.PublicKey{}
	authKeys := [][]byte{}
	key, err := s.ap.GetHostCertificateAuthority()
	if err != nil {
		return nil, err
	}
	authKeys = append(authKeys, key.PublicKey)

	certs, err := s.ap.GetRemoteCertificates(services.HostCert, "")
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		authKeys = append(authKeys, c.PublicKey)
	}
	for _, ak := range authKeys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey(ak)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CA public key '%v', err: %v",
				string(ak), err)
		}
		out = append(out, pk)
	}
	return out, nil
}

func (s *server) keyAuth(
	conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	cid := fmt.Sprintf(
		"reversetunnelconn(%v->%v, user=%v)", conn.RemoteAddr(),
		conn.LocalAddr(), conn.User())

	log.Infof("%v auth attempt with key %v", cid, key.Type())

	err := s.certChecker.CheckHostKey(conn.User(), conn.RemoteAddr(), key)
	if err != nil {
		log.Warningf("reversetunnel(%v->%v, user=%v) ERROR: failed auth user %v, err: %v",
			conn.RemoteAddr(), conn.LocalAddr(), conn.User(), conn.User(), err)
		return nil, err
	}

	if err := s.certChecker.CheckCert(conn.User(), key.(*ssh.Certificate)); err != nil {
		log.Warningf("conn(%v->%v, user=%v) ERROR: Failed to authorize user %v, err: %v",
			conn.RemoteAddr(), conn.LocalAddr(), conn.User(), conn.User(), err)
		return nil, trace.Wrap(err)
	}

	perms := &ssh.Permissions{
		Extensions: map[string]string{
			ExtHost: conn.User(),
		},
	}

	return perms, nil
}

func (s *server) upsertSite(c ssh.Conn) (*remoteSite, error) {
	s.Lock()
	defer s.Unlock()

	domainName := c.User()
	var site *remoteSite
	for _, st := range s.sites {
		if st.domainName == domainName {
			site = st
			break
		}
	}
	if site != nil {
		if err := site.init(c); err != nil {
			return nil, err
		}
	} else {
		site = &remoteSite{srv: s, domainName: c.User()}
		if err := site.init(c); err != nil {
			return nil, err
		}
		s.sites = append(s.sites, site)
	}
	return site, nil
}

func (s *server) GetSites() []RemoteSite {
	s.RLock()
	defer s.RUnlock()
	out := make([]RemoteSite, len(s.sites))
	for i := range s.sites {
		out[i] = s.sites[i]
	}
	return out
}

func (s *server) GetSite(domainName string) (RemoteSite, error) {
	s.RLock()
	defer s.RUnlock()
	for i := range s.sites {
		if s.sites[i].domainName == domainName {
			return s.sites[i], nil
		}
	}
	return nil, fmt.Errorf("site not found")
}

// FindSimilarSite finds the site that is the most similar to domain.
// Returns nil if no sites with such domain name.
func (s *server) FindSimilarSite(domainName string) (RemoteSite, error) {
	s.RLock()
	defer s.RUnlock()

	result := -1
	resultSimilarity := 1

	domainName1 := strings.Split(domainName, ".")
	log.Infof("Find: ", domainName)

	for i, site := range s.sites {
		log.Infof(site.domainName)
		domainName2 := strings.Split(site.domainName, ".")
		similarity := 0
		for j := 1; (j <= len(domainName1)) && (j <= len(domainName2)); j++ {
			if domainName1[len(domainName1)-j] != domainName2[len(domainName2)-j] {
				break
			}
			similarity++
		}
		if (similarity > resultSimilarity) || (result == -1) {
			result = i
			resultSimilarity = similarity
		}
	}

	if result != -1 {
		return s.sites[result], nil
	} else {
		return nil, trace.Errorf("site not found")
	}
}

type remoteSite struct {
	domainName       string
	conn       ssh.Conn
	lastActive time.Time
	srv        *server
	clt        *auth.Client
}

func (s *remoteSite) GetClient() *auth.Client {
	return s.clt
}

func (s *remoteSite) GetEvents(filter events.Filter) ([]lunk.Entry, error) {
	return s.clt.GetEvents(filter)
}

func (s *remoteSite) String() string {
	return fmt.Sprintf("remoteSite(%v)", s.domainName)
}

func (s *remoteSite) init(c ssh.Conn) error {
	if s.conn != nil {
		log.Infof("%v found site, closing previous connection", s)
		s.conn.Close()
	}
	s.conn = c
	tr := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			ch, _, err := s.conn.OpenChannel(chanAccessPoint, nil)
			if err != nil {
				log.Errorf("remoteSite:authProxy %v", err)
				return nil, err
			}
			return newChConn(s.conn, ch), nil
		},
	}
	clt, err := auth.NewClient(
		"http://stub:0",
		roundtrip.HTTPClient(&http.Client{
			Transport: tr,
		}))
	if err != nil {
		return err
	}
	s.clt = clt
	return nil
}

func (s *remoteSite) GetStatus() string {
	diff := time.Now().Sub(s.lastActive)
	if diff > 2*heartbeatPeriod {
		return RemoteSiteStatusOffline
	}
	return RemoteSiteStatusOnline
}

func (s *remoteSite) handleHeartbeat(ch ssh.Channel, reqC <-chan *ssh.Request) {
	go func() {
		for {
			req := <-reqC
			if req == nil {
				log.Infof("agent disconnected")
				return
			}
			log.Infof("%v -> ping", s)
			s.lastActive = time.Now()
		}
	}()
}

func (s *remoteSite) GetName() string {
	return s.domainName
}

func (s *remoteSite) GetLastConnected() time.Time {
	return s.lastActive
}

func (s *remoteSite) ConnectToServer(server, user string, auth []ssh.AuthMethod) (*ssh.Client, error) {
	ch, _, err := s.conn.OpenChannel(chanTransport, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// ask remote channel to dial
	dialed, err := ch.SendRequest(chanTransportDialReq, true, []byte(server))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !dialed {
		return nil, trace.Errorf("remote server %v is not available", server)
	}
	transportConn := newChConn(s.conn, ch)
	conn, chans, reqs, err := ssh.NewClientConn(
		transportConn, server,
		&ssh.ClientConfig{
			User: user,
			Auth: auth,
		})
	if err != nil {
		log.Errorf("remoteSite:connectToServer %v", err)
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func (s *remoteSite) DialServer(server string) (net.Conn, error) {
	serverIsKnown := false
	knownServers, err := s.GetServers()

	for _, srv := range knownServers {
		_, port, err := net.SplitHostPort(srv.Addr)
		if err != nil {
			log.Errorf("server %v(%v) has incorrect address format (%v)",
				srv.Addr, srv.Hostname, err.Error())
		} else {
			if (len(srv.Hostname) != 0) && (len(port) != 0) && (server == srv.Hostname+":"+port) {
				serverIsKnown = true
			}
		}
	}
	if !serverIsKnown {
		return nil, trace.Errorf("can't dial server %v, server is unknown", server)
	}

	ch, _, err := s.conn.OpenChannel(chanTransport, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// ask remote channel to dial
	dialed, err := ch.SendRequest(chanTransportDialReq, true, []byte(server))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if !dialed {
		return nil, trace.Errorf("remote server %v is not available", server)
	}
	return newChConn(s.conn, ch), nil
}

func (s *remoteSite) GetServers() ([]services.Server, error) {
	return s.clt.GetServers()
}

func (s *remoteSite) handleAuthProxy(w http.ResponseWriter, r *http.Request) {
	tr := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			ch, _, err := s.conn.OpenChannel(chanAccessPoint, nil)
			if err != nil {
				log.Errorf("remoteSite:authProxy %v", err)
				return nil, err
			}
			return newChConn(s.conn, ch), nil
		},
	}

	fwd, err := forward.New(forward.RoundTripper(tr), forward.Logger(log.GetLogger()))
	if err != nil {
		log.Errorf("write: %v", err)
		roundtrip.ReplyJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	r.URL.Scheme = "http"
	r.URL.Host = "stub"
	fwd.ServeHTTP(w, r)
}

func newChConn(conn ssh.Conn, ch ssh.Channel) *chConn {
	c := &chConn{}
	c.Channel = ch
	c.conn = conn
	return c
}

type chConn struct {
	ssh.Channel
	conn ssh.Conn
}

func (c *chConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *chConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *chConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *chConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *chConn) SetWriteDeadline(t time.Time) error {
	return nil
}

const ExtHost = "host@teleport"

package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Intercept TLS (HTTPS) connections.

var certFile = flag.String("tls-cert", "", "path to certificate for serving HTTPS")
var keyFile = flag.String("tls-key", "", "path to TLS certificate key")
var sslBypassFile = newActiveFlag("tls-bypass", "", "path to list of sites that bypass SSLBump", tlsBypass.Load)
var sslBumpFile = newActiveFlag("tls-bump-only", "", "path to a list of the only sites that should be SSLBumped", loadTlsBumpOnly)

var tlsCert tls.Certificate
var parsedTLSCert *x509.Certificate
var tlsReady bool

var tlsBypass = NewHostList()
var tlsBumpOnly *HostList

// unverifiedClientConfig is a TLS configuration that doesn't verify server
// certificates.
var unverifiedClientConfig = &tls.Config{
	InsecureSkipVerify: true,
}

// loadCertificate loads the TLS certificate specified by certFile and keyFile
// into tlsCert.
func loadCertificate() {
	if *certFile != "" && *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Println("Error loading TLS certificate:", err)
			return
		}
		tlsCert = cert
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			log.Println("Error parsing X509 certificate:", err)
			return
		}
		parsedTLSCert = parsed
		tlsReady = true

		http.HandleFunc("/cert.der", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			w.Write(tlsCert.Certificate[len(tlsCert.Certificate)-1])
		})
	}
}

// connectDirect connects to serverAddr and copies data between it and conn.
// extraData is sent to the server first.
func connectDirect(conn net.Conn, serverAddr string, extraData []byte) {
	activeConnections.Add(1)
	defer activeConnections.Done()

	serverConn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		log.Printf("error with pass-through of SSL connection to %s: %s", serverAddr, err)
		return
	}

	if extraData != nil {
		// There may also be data waiting in the socket's input buffer;
		// read it before we send the data on, so that the first packet of
		// the connection doesn't get split in two.
		conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		buf := make([]byte, 2000)
		n, _ := conn.Read(buf)
		conn.SetReadDeadline(time.Time{})
		if n > 0 {
			extraData = append(extraData, buf[:n]...)
		}
		serverConn.Write(extraData)
	}

	go func() {
		io.Copy(conn, serverConn)
		time.Sleep(time.Second)
		conn.Close()
	}()
	io.Copy(serverConn, conn)
	serverConn.Close()
	return
}

// SSLBump performs a man-in-the-middle attack on conn, to filter the HTTPS
// traffic. serverAddr is the address (host:port) of the server the client was
// trying to connect to. user is the name of an already-authenticated user.
func SSLBump(conn net.Conn, serverAddr, user string) {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, 4096)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("SSLBump: panic serving connection to %s: %v\n%s", serverAddr, err, buf)
			conn.Close()
		}
	}()

	if serverAddr == localServer+":443" {
		// The internal server gets special treatment, since there is no remote
		// server to connect to.
		cert, err := imitateCertificate(&x509.Certificate{
			Subject:     pkix.Name{CommonName: localServer},
			NotBefore:   parsedTLSCert.NotBefore,
			NotAfter:    parsedTLSCert.NotAfter,
			KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}, false)
		if err != nil {
			log.Printf("Error generataing HTTPS certificate for local server (%s): %v", serverAddr, err)
			conn.Close()
			return
		}

		config := &tls.Config{
			NextProtos:   []string{"http/1.1"},
			Certificates: []tls.Certificate{cert, tlsCert},
		}
		tlsConn := tls.Server(conn, config)
		err = tlsConn.Handshake()
		if err != nil {
			logTLS(user, serverAddr, localServer, fmt.Errorf("error in handshake with client: %v", err))
			conn.Close()
			return
		}
		listener := &singleListener{conn: tlsConn}
		server := http.Server{}
		logTLS(user, serverAddr, localServer, nil)
		server.Serve(listener)
		return
	}

	// Read the client hello so that we can find out the name of the server (not
	// just the address).
	clientHello, err := readClientHello(conn)
	if err != nil {
		logTLS(user, serverAddr, "", fmt.Errorf("error reading client hello: %v", err))
		connectDirect(conn, serverAddr, clientHello)
		return
	}

	serverName, ok := clientHelloServerName(clientHello)
	if !ok || serverName == "" {
		serverName, _, err = net.SplitHostPort(serverAddr)
		if err != nil {
			serverName = serverAddr
		}

		if ip := net.ParseIP(serverName); ip != nil {
			// All we have is an IP address, not a name from a CONNECT request.
			// See if we can do better by reverse DNS.
			names, err := net.LookupAddr(serverName)
			if err == nil && len(names) > 0 {
				serverName = strings.TrimSuffix(names[0], ".")
			}
		}
	}

	if tlsBypass.Contains(serverName) {
		logTLS(user, serverAddr, serverName, errors.New("site bypass"))
		connectDirect(conn, serverAddr, clientHello)
		return
	}
	if !tlsReady || (tlsBumpOnly != nil && !tlsBumpOnly.Contains(serverName)) {
		// Do hostname filtering; if the host is allowed, let the client connect directly.
		u := &url.URL{Host: serverName}
		sc := scorecard{tally: URLRules.MatchingRules(u)}
		sc.calculate(user)
		req, _ := http.NewRequest("CONNECT", u.String(), nil)
		logAccess(req, nil, sc, "", 0, false, user)
		if sc.action == BLOCK {
			conn.Close()
		} else {
			connectDirect(conn, serverAddr, clientHello)
		}
		return
	}

	serverConn, err := tls.Dial("tcp", serverAddr, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true,
	})
	if err != nil {
		logTLS(user, serverAddr, serverName, err)
		connectDirect(conn, serverAddr, clientHello)
		return
	}

	state := serverConn.ConnectionState()
	serverCert := state.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, ic := range state.PeerCertificates[1:] {
		intermediates.AddCert(ic)
	}

	valid := validCert(serverCert, intermediates)
	cert, err := imitateCertificate(serverCert, !valid)
	if err != nil {
		serverConn.Close()
		logTLS(user, serverAddr, serverName, fmt.Errorf("error generating certificate: %v", err))
		connectDirect(conn, serverAddr, clientHello)
		return
	}

	config := &tls.Config{
		NextProtos:   []string{"http/1.1"},
		Certificates: []tls.Certificate{cert, tlsCert},
	}

	tlsConn := tls.Server(&insertingConn{conn, clientHello}, config)
	err = tlsConn.Handshake()
	if err != nil {
		logTLS(user, serverAddr, serverName, fmt.Errorf("error in handshake with client: %v", err))
		conn.Close()
		return
	}

	_, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		port = ""
	}
	listener := &singleListener{conn: tlsConn}
	server := http.Server{
		Handler: proxyHandler{
			TLS:         true,
			connectPort: port,
			user:        user,
			rt:          NewTLSRedialTransport(serverConn, serverName),
		},
	}
	logTLS(user, serverAddr, serverName, nil)
	server.Serve(listener)
}

// A insertingConn is a net.Conn that inserts extra data at the start of the
// incoming data stream.
type insertingConn struct {
	net.Conn
	extraData []byte
}

func (c *insertingConn) Read(p []byte) (n int, err error) {
	if len(c.extraData) == 0 {
		return c.Conn.Read(p)
	}

	n = copy(p, c.extraData)
	c.extraData = c.extraData[n:]
	return
}

// A singleListener is a net.Listener that returns a single connection, then
// gives the error io.EOF.
type singleListener struct {
	conn net.Conn
	once sync.Once
}

func (s *singleListener) Accept() (net.Conn, error) {
	var c net.Conn
	s.once.Do(func() {
		c = s.conn
	})
	if c != nil {
		return c, nil
	}
	return nil, io.EOF
}

func (s *singleListener) Close() error {
	s.once.Do(func() {
		s.conn.Close()
	})
	return nil
}

func (s *singleListener) Addr() net.Addr {
	return s.conn.LocalAddr()
}

// imitateCertificate returns a new TLS certificate that has most of the same
// data as serverCert but is signed by Redwood's root certificate, or
// self-signed.
func imitateCertificate(serverCert *x509.Certificate, selfSigned bool) (cert tls.Certificate, err error) {
	template := serverCert

	if selfSigned {
		template = &x509.Certificate{
			SerialNumber: new(big.Int).SetInt64(0),
			Subject:      serverCert.Subject,
			NotBefore:    serverCert.NotBefore,
			NotAfter:     serverCert.NotAfter,
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  serverCert.ExtKeyUsage,
			DNSNames:     serverCert.DNSNames,
		}
	} else {
		// Use a hash of the real certificate as the serial number.
		h := md5.New()
		h.Write(serverCert.Raw)
		h.Write([]byte{0}) // To give different serial numbers after the key usage change.
		template.SerialNumber = big.NewInt(0).SetBytes(h.Sum(nil))
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("failed to generate serial number: %s", err)
		}
		template.SubjectKeyId = nil
		template.AuthorityKeyId = nil
		template.OCSPServer = nil
		template.IssuingCertificateURL = nil
		template.CRLDistributionPoints = nil
		template.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}

	var newCertBytes []byte
	if selfSigned {
		newCertBytes, err = x509.CreateCertificate(rand.Reader, template, template, parsedTLSCert.PublicKey, tlsCert.PrivateKey)
	} else {
		newCertBytes, err = x509.CreateCertificate(rand.Reader, template, parsedTLSCert, parsedTLSCert.PublicKey, tlsCert.PrivateKey)
	}
	if err != nil {
		return tls.Certificate{}, err
	}

	newCert := tls.Certificate{
		Certificate: [][]byte{newCertBytes},
		PrivateKey:  tlsCert.PrivateKey,
	}

	if !selfSigned {
		newCert.Certificate = append(newCert.Certificate, tlsCert.Certificate...)
	}
	return newCert, nil
}

func validCert(cert *x509.Certificate, intermediates *x509.CertPool) bool {
	_, err := cert.Verify(x509.VerifyOptions{Intermediates: intermediates})
	if err == nil {
		return true
	}
	if _, ok := err.(x509.UnknownAuthorityError); !ok {
		// There was an error, but not because the certificate wasn't signed
		// by a recognized CA. So we go ahead and use the cert and let
		// the client experience the same error.
		return true
	}

	// Before we give up, we'll try fetching some intermediate certificates.
	if len(cert.IssuingCertificateURL) == 0 {
		return false
	}

	toFetch := cert.IssuingCertificateURL
	fetched := make(map[string]bool)

	for i := 0; i < len(toFetch); i++ {
		certURL := toFetch[i]
		if fetched[certURL] {
			continue
		}
		resp, err := http.Get(certURL)
		if err == nil {
			defer resp.Body.Close()
		}
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		fetchedCert, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			continue
		}

		// The fetched certificate might be in either DER or PEM format.
		if bytes.Contains(fetchedCert, []byte("-----BEGIN CERTIFICATE-----")) {
			// It's PEM.
			var certDER *pem.Block
			for {
				certDER, fetchedCert = pem.Decode(fetchedCert)
				if certDER == nil {
					break
				}
				if certDER.Type != "CERTIFICATE" {
					continue
				}
				thisCert, err := x509.ParseCertificate(certDER.Bytes)
				if err != nil {
					continue
				}
				intermediates.AddCert(thisCert)
				toFetch = append(toFetch, thisCert.IssuingCertificateURL...)
			}
		} else {
			// Hopefully it's DER.
			thisCert, err := x509.ParseCertificate(fetchedCert)
			if err != nil {
				continue
			}
			intermediates.AddCert(thisCert)
			toFetch = append(toFetch, thisCert.IssuingCertificateURL...)
		}
	}

	_, err = cert.Verify(x509.VerifyOptions{Intermediates: intermediates})
	if err == nil {
		return true
	}
	if _, ok := err.(x509.UnknownAuthorityError); !ok {
		// There was an error, but not because the certificate wasn't signed
		// by a recognized CA. So we go ahead and use the cert and let
		// the client experience the same error.
		return true
	}
	return false
}

func readClientHello(conn net.Conn) (hello []byte, err error) {
	var header [5]byte
	n, err := io.ReadFull(conn, header[:])
	hello = header[:n]
	if err != nil {
		return hello, err
	}

	if header[0] != 22 {
		return hello, fmt.Errorf("expected content type of 22, got %d", header[0])
	}
	if header[1] != 3 {
		return hello, fmt.Errorf("expected major version of 3, got %d", header[1])
	}
	recordLen := int(header[3])<<8 | int(header[4])
	if recordLen > 0x3000 {
		return hello, fmt.Errorf("expected length less than 12kB, got %d", recordLen)
	}
	if recordLen < 4 {
		return hello, fmt.Errorf("expected length of at least 4 bytes, got %d", recordLen)
	}

	protocolData := make([]byte, recordLen)
	n, err = io.ReadFull(conn, protocolData)
	hello = append(hello, protocolData[:n]...)
	if err != nil {
		return hello, err
	}
	if protocolData[0] != 1 {
		return hello, fmt.Errorf("Expected message type 1 (ClientHello), got %d", protocolData[0])
	}
	protocolLen := int(protocolData[1])<<16 | int(protocolData[2])<<8 | int(protocolData[3])
	if protocolLen != recordLen-4 {
		return hello, fmt.Errorf("recordLen=%d, protocolLen=%d", recordLen, protocolLen)
	}

	return hello, nil
}

func clientHelloServerName(data []byte) (name string, ok bool) {
	if len(data) < 5 {
		return "", false
	}
	// Strip off the record header.
	data = data[5:]

	if len(data) < 42 {
		return "", false
	}

	sessionIdLen := int(data[38])
	if sessionIdLen > 32 || len(data) < 39+sessionIdLen {
		return "", false
	}
	data = data[39+sessionIdLen:]
	if len(data) < 2 {
		return "", false
	}

	cipherSuiteLen := int(data[0])<<8 | int(data[1])
	if cipherSuiteLen%2 == 1 || len(data) < 2+cipherSuiteLen {
		return "", false
	}
	data = data[2+cipherSuiteLen:]
	if len(data) < 1 {
		return "", false
	}

	compressionMethodsLen := int(data[0])
	if len(data) < 1+compressionMethodsLen {
		return "", false
	}
	data = data[1+compressionMethodsLen:]
	if len(data) < 2 {
		return "", false
	}

	extensionsLength := int(data[0])<<8 | int(data[1])
	data = data[2:]
	if extensionsLength != len(data) {
		return "", false
	}

	for len(data) != 0 {
		if len(data) < 4 {
			return "", false
		}
		extension := uint16(data[0])<<8 | uint16(data[1])
		length := int(data[2])<<8 | int(data[3])
		data = data[4:]
		if len(data) < length {
			return "", false
		}

		if extension == 0 /* server name */ {
			if length < 2 {
				return "", false
			}
			numNames := int(data[0])<<8 | int(data[1])
			d := data[2:]
			for i := 0; i < numNames; i++ {
				if len(d) < 3 {
					return "", false
				}
				nameType := d[0]
				nameLen := int(d[1])<<8 | int(d[2])
				d = d[3:]
				if len(d) < nameLen {
					return "", false
				}
				if nameType == 0 {
					return string(d[:nameLen]), true
				}
				d = d[nameLen:]
			}
		}

		data = data[length:]
	}

	return "", true
}

// addressToServerName is a cache of the CN names returned by the HTTPS servers
// at various addresses.
var addressToServerName = make(map[string]string)
var addressToServerNameLock sync.RWMutex

func serverNameAtAddress(addr string) string {
	addressToServerNameLock.RLock()
	name, ok := addressToServerName[addr]
	addressToServerNameLock.RUnlock()
	if ok {
		return name
	}

	name = getServerNameAtAddress(addr)
	addressToServerNameLock.Lock()
	addressToServerName[addr] = name
	addressToServerNameLock.Unlock()

	return name
}

// getServerNameAtAddress attempts to connect to the server at addr and return
// the Common Name of its TLS certificate.
func getServerNameAtAddress(addr string) string {
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		host, _, err := net.SplitHostPort(addr)
		if err == nil {
			return host
		} else {
			return addr
		}
	}
	defer conn.Close()
	state := conn.ConnectionState()
	serverCert := state.PeerCertificates[0]
	return serverCert.Subject.CommonName
}

func loadTlsBumpOnly(filename string) error {
	if tlsBumpOnly == nil {
		tlsBumpOnly = NewHostList()
	}
	return tlsBumpOnly.Load(filename)
}

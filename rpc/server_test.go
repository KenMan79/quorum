// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/jpmorganchase/quorum-security-plugin-sdk-go/proto"
	"github.com/stretchr/testify/assert"
)

func TestServerRegisterName(t *testing.T) {
	server := NewServer()
	service := new(testService)

	if err := server.RegisterName("test", service); err != nil {
		t.Fatalf("%v", err)
	}

	if len(server.services.services) != 2 {
		t.Fatalf("Expected 2 service entries, got %d", len(server.services.services))
	}

	svc, ok := server.services.services["test"]
	if !ok {
		t.Fatalf("Expected service calc to be registered")
	}

	wantCallbacks := 9
	// Quorum - Add extra callback for the function added by us EchoCtxId
	wantCallbacks += 1
	// End Quorum
	if len(svc.callbacks) != wantCallbacks {
		t.Errorf("Expected %d callbacks for service 'service', got %d", wantCallbacks, len(svc.callbacks))
	}
}

func TestServer(t *testing.T) {
	files, err := ioutil.ReadDir("testdata")
	if err != nil {
		t.Fatal("where'd my testdata go?")
	}
	for _, f := range files {
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
			continue
		}
		path := filepath.Join("testdata", f.Name())
		name := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
		t.Run(name, func(t *testing.T) {
			runTestScript(t, path)
		})
	}
}

func runTestScript(t *testing.T, file string) {
	server := newTestServer()
	content, err := ioutil.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	go server.ServeCodec(NewCodec(serverConn), 0)
	readbuf := bufio.NewReader(clientConn)
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case len(line) == 0 || strings.HasPrefix(line, "//"):
			// skip comments, blank lines
			continue
		case strings.HasPrefix(line, "--> "):
			t.Log(line)
			// write to connection
			clientConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(clientConn, line[4:]+"\n"); err != nil {
				t.Fatalf("write error: %v", err)
			}
		case strings.HasPrefix(line, "<-- "):
			t.Log(line)
			want := line[4:]
			// read line from connection and compare text
			clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			sent, err := readbuf.ReadString('\n')
			if err != nil {
				t.Fatalf("read error: %v", err)
			}
			sent = strings.TrimRight(sent, "\r\n")
			if sent != want {
				t.Errorf("wrong line from server\ngot:  %s\nwant: %s", sent, want)
			}
		default:
			panic("invalid line in test script: " + line)
		}
	}
}

// This test checks that responses are delivered for very short-lived connections that
// only carry a single request.
func TestServerShortLivedConn(t *testing.T) {
	server := newTestServer()
	defer server.Stop()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("can't listen:", err)
	}
	defer listener.Close()
	go server.ServeListener(listener)

	var (
		request  = `{"jsonrpc":"2.0","id":1,"method":"rpc_modules"}` + "\n"
		wantResp = `{"jsonrpc":"2.0","id":1,"result":{"nftest":"1.0","rpc":"1.0","test":"1.0"}}` + "\n"
		deadline = time.Now().Add(10 * time.Second)
	)
	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal("can't dial:", err)
		}
		defer conn.Close()
		conn.SetDeadline(deadline)
		// Write the request, then half-close the connection so the server stops reading.
		conn.Write([]byte(request))
		conn.(*net.TCPConn).CloseWrite()
		// Now try to get the response.
		buf := make([]byte, 2000)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatal("read error:", err)
		}
		if !bytes.Equal(buf[:n], []byte(wantResp)) {
			t.Fatalf("wrong response: %s", buf[:n])
		}
	}
}

func TestAuthenticateHttpRequest_whenAuthenticationManagerFails(t *testing.T) {
	protectedServer := NewProtectedServer(&stubAuthenticationManager{false, errors.New("arbitrary error")})
	arbitraryRequest, _ := http.NewRequest("POST", "https://arbitraryUrl", nil)
	captor := &securityContextConfigurerCaptor{}

	protectedServer.authenticateHttpRequest(arbitraryRequest, captor)

	actualErr, hasError := captor.context.Value(ctxAuthenticationError).(error)
	assert.True(t, hasError, "must have error")
	assert.EqualError(t, actualErr, "internal error")
	_, hasAuthToken := captor.context.Value(CtxPreauthenticatedToken).(*proto.PreAuthenticatedAuthenticationToken)
	assert.False(t, hasAuthToken, "must not be preauthenticated")
}

func TestAuthenticateHttpRequest_whenTypical(t *testing.T) {
	protectedServer := NewProtectedServer(&stubAuthenticationManager{true, nil})
	arbitraryRequest, _ := http.NewRequest("POST", "https://arbitraryUrl", nil)
	arbitraryRequest.Header.Set(HttpAuthorizationHeader, "arbitrary value")
	captor := &securityContextConfigurerCaptor{}

	protectedServer.authenticateHttpRequest(arbitraryRequest, captor)

	_, hasError := captor.context.Value(ctxAuthenticationError).(error)
	assert.False(t, hasError, "must not have error")
	_, hasAuthToken := captor.context.Value(CtxPreauthenticatedToken).(*proto.PreAuthenticatedAuthenticationToken)
	assert.True(t, hasAuthToken, "must be preauthenticated")
}

func TestAuthenticateHttpRequest_whenAuthenticationManagerIsDisabled(t *testing.T) {
	protectedServer := NewProtectedServer(&stubAuthenticationManager{false, nil})
	arbitraryRequest, _ := http.NewRequest("POST", "https://arbitraryUrl", nil)
	captor := &securityContextConfigurerCaptor{}

	protectedServer.authenticateHttpRequest(arbitraryRequest, captor)

	_, hasError := captor.context.Value(ctxAuthenticationError).(error)
	assert.False(t, hasError, "must not have error")
	_, hasAuthToken := captor.context.Value(CtxPreauthenticatedToken).(*proto.PreAuthenticatedAuthenticationToken)
	assert.False(t, hasAuthToken, "must not be preauthenticated")
}

func TestAuthenticateHttpRequest_whenMissingAccessToken(t *testing.T) {
	protectedServer := NewProtectedServer(&stubAuthenticationManager{true, nil})
	arbitraryRequest, _ := http.NewRequest("POST", "https://arbitraryUrl", nil)
	captor := &securityContextConfigurerCaptor{}

	protectedServer.authenticateHttpRequest(arbitraryRequest, captor)

	actualErr, hasError := captor.context.Value(ctxAuthenticationError).(error)
	assert.True(t, hasError, "must have error")
	assert.EqualError(t, actualErr, "missing access token")
	_, hasAuthToken := captor.context.Value(CtxPreauthenticatedToken).(*proto.PreAuthenticatedAuthenticationToken)
	assert.False(t, hasAuthToken, "must not be preauthenticated")
}

type securityContextConfigurerCaptor struct {
	context securityContext
}

func (sc *securityContextConfigurerCaptor) Configure(secCtx securityContext) {
	sc.context = secCtx
}

type stubAuthenticationManager struct {
	isEnabled bool
	stubErr   error
}

func (s *stubAuthenticationManager) Authenticate(_ context.Context, _ string) (*proto.PreAuthenticatedAuthenticationToken, error) {
	expiredAt, err := ptypes.TimestampProto(time.Now().Add(1 * time.Hour))
	if err != nil {
		return nil, err
	}
	return &proto.PreAuthenticatedAuthenticationToken{
		ExpiredAt: expiredAt,
	}, nil
}

func (s *stubAuthenticationManager) IsEnabled(_ context.Context) (bool, error) {
	return s.isEnabled, s.stubErr
}

// Quorum - This test checks that the `ID` from the RPC call is passed to the handler method
func TestServerContextIdCaptured(t *testing.T) {
	var (
		request  = `{"jsonrpc":"2.0","id":1,"method":"test_echoCtxId"}` + "\n"
		wantResp = `{"jsonrpc":"2.0","id":1,"result":1}` + "\n"
	)

	server := newTestServer()
	defer server.Stop()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("can't listen:", err)
	}
	defer listener.Close()
	go server.ServeListener(listener)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("can't dial:", err)
	}
	defer conn.Close()
	// Write the request, then half-close the connection so the server stops reading.
	conn.Write([]byte(request))
	conn.(*net.TCPConn).CloseWrite()
	// Now try to get the response.
	buf := make([]byte, 2000)
	n, err := conn.Read(buf)

	assert.NoErrorf(t, err, "read error:", err)
	assert.Equalf(t, buf[:n], []byte(wantResp), "wrong response: %s", buf[:n])
}

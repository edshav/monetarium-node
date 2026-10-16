// Copyright (c) 2016 The btcsuite developers
// Copyright (c) 2017-2022 The Decred developers

// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monetarium/monetarium-node/rpc/jsonrpc/types"
)

func TestCheckAuthUserPass(t *testing.T) {
	s, err := New(&Config{
		RPCUser:      "user",
		RPCPass:      "pass",
		RPCLimitUser: "limit",
		RPCLimitPass: "limit",
	})
	if err != nil {
		t.Fatalf("unable to create RPC server: %v", err)
	}
	tests := []struct {
		name       string
		user       string
		pass       string
		wantAuthed bool
		wantAdmin  bool
	}{
		{
			name:       "correct admin",
			user:       "user",
			pass:       "pass",
			wantAuthed: true,
			wantAdmin:  true,
		},
		{
			name:       "correct limited user",
			user:       "limit",
			pass:       "limit",
			wantAuthed: true,
			wantAdmin:  false,
		},
		{
			name:       "invalid admin",
			user:       "user",
			pass:       "p",
			wantAuthed: false,
			wantAdmin:  false,
		},
		{
			name:       "invalid limited user",
			user:       "limit",
			pass:       "",
			wantAuthed: false,
			wantAdmin:  false,
		},
		{
			name:       "invalid empty user",
			user:       "",
			pass:       "",
			wantAuthed: false,
			wantAdmin:  false,
		},
	}
	for _, test := range tests {
		authed, isAdmin := s.checkAuthUserPass(test.user, test.pass, "addr")
		if authed != test.wantAuthed {
			t.Errorf("%q: unexpected authed -- got %v, want %v", test.name, authed,
				test.wantAuthed)
		}
		if isAdmin != test.wantAdmin {
			t.Errorf("%q: unexpected isAdmin -- got %v, want %v", test.name, isAdmin,
				test.wantAdmin)
		}
	}
}

func TestCheckAuth(t *testing.T) {
	{
		s, err := New(&Config{})
		if err != nil {
			t.Fatalf("unable to create RPC server: %v", err)
		}
		for i := 0; i <= 1; i++ {
			authed, isAdmin, err := s.checkAuth(&http.Request{}, i == 0)
			if !authed {
				t.Errorf(" unexpected authed -- got %v, want %v", authed, true)
			}
			if !isAdmin {
				t.Errorf("unexpected isAdmin -- got %v, want %v", isAdmin, true)
			}
			if err != nil {
				t.Errorf("unexpected err -- got %v, want %v", err, nil)
			}
		}
	}
	{
		s, err := New(&Config{
			RPCUser:      "user",
			RPCPass:      "pass",
			RPCLimitUser: "limit",
			RPCLimitPass: "limit",
		})
		if err != nil {
			t.Fatalf("unable to create RPC server: %v", err)
		}
		for i := 0; i <= 1; i++ {
			authed, isAdmin, err := s.checkAuth(&http.Request{}, i == 0)
			if authed {
				t.Errorf(" unexpected authed -- got %v, want %v", authed, false)
			}
			if isAdmin {
				t.Errorf("unexpected isAdmin -- got %v, want %v", isAdmin, false)
			}
			if i == 0 && err == nil {
				t.Errorf("unexpected err -- got %v, want auth failure", err)
			} else if i != 0 && err != nil {
				t.Errorf("unexpected err -- got %v, want <nil>", err)
			}
		}
		for i := 0; i <= 1; i++ {
			r := &http.Request{Header: make(map[string][]string, 1)}
			r.Header["Authorization"] = []string{"Basic Nothing"}
			authed, isAdmin, err := s.checkAuth(r, i == 0)
			if authed {
				t.Errorf(" unexpected authed -- got %v, want %v", authed, false)
			}
			if isAdmin {
				t.Errorf("unexpected isAdmin -- got %v, want %v", isAdmin, false)
			}
			if err == nil {
				t.Errorf("unexpected err -- got %v, want auth failure", err)
			}
		}
	}
}

// TestLimitedMethodsExist ensures all RPC methods listed in the limited RPC
// methods map have associated handlers defined.
func TestLimitedMethodsExist(t *testing.T) {
	for methodStr := range rpcLimited {
		method := types.Method(methodStr)
		_, haveRegularHandler := rpcHandlers[method]
		_, haveWebsocketHandler := wsHandlers[method]
		if !haveRegularHandler && !haveWebsocketHandler {
			t.Errorf("no handler found for limited method %q", method)
		}
	}
}

// TestWebsocketCheckOrigin ensures the websocket endpoint only accepts
// handshakes whose origin host matches the request host, ignoring ports.
func TestWebsocketCheckOrigin(t *testing.T) {
	s, err := New(&Config{})
	if err != nil {
		t.Fatalf("unable to create RPC server: %v", err)
	}
	handler := s.route(context.Background()).Handler

	tests := []struct {
		name   string
		origin string
		host   string
		accept bool
	}{{
		name:   "no origin",
		host:   "node.example",
		accept: true,
	}, {
		name:   "mismatched hosts without ports",
		origin: "https://evil.example",
		host:   "node.example",
		accept: false,
	}, {
		name:   "mismatched hosts with ports",
		origin: "https://evil.example:443",
		host:   "node.example:9509",
		accept: false,
	}, {
		name:   "matching hosts without ports",
		origin: "https://node.example",
		host:   "node.example",
		accept: true,
	}, {
		name:   "matching hosts with different ports",
		origin: "https://node.example:443",
		host:   "node.example:9509",
		accept: true,
	}, {
		name:   "matching hosts with port on request host only",
		origin: "https://node.example",
		host:   "node.example:9509",
		accept: true,
	}, {
		name:   "matching hosts with different case",
		origin: "https://Node.Example",
		host:   "node.example",
		accept: true,
	}, {
		name:   "matching IPv6 hosts with different ports",
		origin: "https://[::1]:443",
		host:   "[::1]:9509",
		accept: true,
	}, {
		name:   "mismatched IPv6 hosts with ports",
		origin: "https://[::2]:443",
		host:   "[::1]:9509",
		accept: false,
	}}

	for _, test := range tests {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Host = test.host
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}

		// The upgrader rejects a disallowed origin with 403.  An allowed origin
		// proceeds to hijack the connection, which the recorder does not
		// support, so it fails with 500 instead.
		want := http.StatusForbidden
		if test.accept {
			want = http.StatusInternalServerError
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: unexpected status -- got %d, want %d", test.name,
				w.Code, want)
		}
	}
}

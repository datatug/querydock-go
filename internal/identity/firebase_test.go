package identity

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"github.com/datatug/querydock-go/internal/publishing"
	"google.golang.org/api/option"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type tokenFunc func(context.Context, string) (*auth.Token, error)

func (f tokenFunc) VerifyIDTokenAndCheckRevoked(c context.Context, s string) (*auth.Token, error) {
	return f(c, s)
}
func emptyEnv(string) string { return "" }
func validToken() *auth.Token {
	return &auth.Token{UID: "real-owner", Subject: "real-owner", Audience: ProjectID, Issuer: "https://securetoken.google.com/" + ProjectID, Firebase: auth.FirebaseInfo{SignInProvider: "password"}}
}
func TestVerifierMapping(t *testing.T) {
	for _, kind := range []string{"valid", "anonymous", "nil", "uid", "subject", "audience", "issuer", "provider", "lookup-error", "emulator-later"} {
		t.Run(kind, func(t *testing.T) {
			token := validToken()
			var sdkErr error
			want := publishing.ErrUnauthorized
			getenv := emptyEnv
			switch kind {
			case "valid":
				want = nil
			case "anonymous":
				token.Firebase.SignInProvider = "anonymous"
				want = nil
			case "nil":
				token = nil
			case "uid":
				token.UID = ""
			case "subject":
				token.Subject = "another"
			case "audience":
				token.Audience = "other-project"
			case "issuer":
				token.Issuer = "forged"
			case "provider":
				token.Firebase.SignInProvider = ""
			case "lookup-error":
				sdkErr = errors.New("private service detail")
				want = publishing.ErrUnavailable
			case "emulator-later":
				getenv = func(string) string { return "127.0.0.1:1" }
				want = publishing.ErrUnavailable
			}
			called := false
			v := Verifier{client: tokenFunc(func(_ context.Context, s string) (*auth.Token, error) {
				called = true
				if s != "credential" {
					t.Fatal("credential changed")
				}
				return token, sdkErr
			}), project: ProjectID, getenv: getenv}
			identity, err := v.Verify(context.Background(), "credential")
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if want == nil && (identity.UID != "real-owner" || identity.Anonymous != (kind == "anonymous")) {
				t.Fatal("incorrect identity")
			}
			if kind == "emulator-later" && called {
				t.Fatal("emulator switch reached SDK")
			}
			if err != nil && identity != (publishing.Identity{}) {
				t.Fatal("authority returned with error")
			}
		})
	}
}
func TestConstructorGuards(t *testing.T) {
	for _, kind := range []string{"wrong-project", "auth-emulator", "firestore-emulator", "factory-error", "nil-client", "valid"} {
		t.Run(kind, func(t *testing.T) {
			project := ProjectID
			getenv := emptyEnv
			if kind == "wrong-project" {
				project = "caller-project"
			}
			if kind == "auth-emulator" || kind == "firestore-emulator" {
				getenv = func(key string) string {
					if (kind == "auth-emulator" && key == "FIREBASE_AUTH_EMULATOR_HOST") || (kind == "firestore-emulator" && key == "FIRESTORE_EMULATOR_HOST") {
						return "localhost:1"
					}
					return ""
				}
			}
			called := false
			v, err := newVerifier(context.Background(), project, getenv, func(_ context.Context, p string) (tokenClient, error) {
				called = true
				if p != ProjectID {
					t.Fatal("wrong audience reached SDK")
				}
				if kind == "factory-error" {
					return nil, errors.New("credential detail")
				}
				if kind == "nil-client" {
					return nil, nil
				}
				return tokenFunc(func(context.Context, string) (*auth.Token, error) { return validToken(), nil }), nil
			})
			if kind == "valid" {
				if err != nil || v == nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, publishing.ErrUnavailable) || v != nil {
					t.Fatal("constructor failed open")
				}
				if (kind == "wrong-project" || strings.Contains(kind, "emulator")) && called {
					t.Fatal("unsafe config reached SDK")
				}
			}
		})
	}
	if _, err := NewProduction(context.Background(), "wrong"); !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("production accepted wrong project")
	}
}
func TestSDKFactoryErrors(t *testing.T) {
	oldApp, oldAuth := newApp, appAuth
	t.Cleanup(func() { newApp, appAuth = oldApp, oldAuth })
	newApp = func(context.Context, *firebase.Config, ...option.ClientOption) (*firebase.App, error) {
		return nil, errors.New("private credential error")
	}
	if _, err := sdkClient(context.Background(), ProjectID); !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("app error leaked")
	}
	newApp = func(c context.Context, cfg *firebase.Config, opts ...option.ClientOption) (*firebase.App, error) {
		return oldApp(c, cfg, option.WithoutAuthentication())
	}
	appAuth = func(*firebase.App, context.Context) (*auth.Client, error) {
		return nil, errors.New("private auth config")
	}
	if _, err := sdkClient(context.Background(), ProjectID); !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("auth error leaked")
	}
	appAuth = oldAuth
	client, err := sdkClient(context.Background(), ProjectID)
	if err != nil || client == nil {
		t.Fatal(err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jwt(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	text := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	hash := crypto.SHA256.New()
	hash.Write([]byte(text))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return text + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func TestActualSDKSignatureAndRevocation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-only"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	certJSON, _ := json.Marshal(map[string]string{"test-key": cert})
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	lookupCount := 0
	disabled := false
	validSince := "0"
	lookupFailure := false
	responder := roundTrip(func(r *http.Request) (*http.Response, error) {
		var response string
		status := 200
		switch {
		case r.URL.Host == "www.googleapis.com" && strings.Contains(r.URL.Path, "/robot/v1/metadata/x509/"):
			response = string(certJSON)
		case r.URL.Host == "identitytoolkit.googleapis.com" && strings.HasSuffix(r.URL.Path, "accounts:lookup"):
			lookupCount++
			if lookupFailure {
				response = `{"error":{"code":403,"message":"INSUFFICIENT_PERMISSION"}}`
				status = 403
			} else {
				data, _ := json.Marshal(map[string]any{"users": []any{map[string]any{"localId": "real-owner", "disabled": disabled, "validSince": validSince}}})
				response = string(data)
			}
		default:
			t.Fatalf("unexpected SDK network target %s", r.URL)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"max-age=3600"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	var connections sync.WaitGroup
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test refuses unowned network")
	}, DialTLSContext: func(_ context.Context, _, address string) (net.Conn, error) {
		client, server := net.Pipe()
		connections.Add(1)
		go func() {
			defer connections.Done()
			defer func() {
				if err := server.Close(); err != nil {
					t.Errorf("close owned pipe: %v", err)
				}
			}()
			_ = server.SetDeadline(time.Now().Add(5 * time.Second))
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			request.URL.Host = request.Host
			request.URL.Scheme = "https"
			_, _ = io.Copy(io.Discard, request.Body)
			_ = request.Body.Close()
			response, err := responder.RoundTrip(request)
			if err != nil {
				return
			}
			data, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(string(data)))
			response.ContentLength = int64(len(data))
			response.Close = true
			_ = response.Write(server)
		}()
		return client, nil
	}}
	http.DefaultTransport = transport
	t.Cleanup(func() { transport.CloseIdleConnections(); connections.Wait() })
	oldApp := newApp
	t.Cleanup(func() { newApp = oldApp })
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "")
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	newApp = func(c context.Context, cfg *firebase.Config, _ ...option.ClientOption) (*firebase.App, error) {
		return oldApp(c, cfg, option.WithoutAuthentication())
	}
	v, err := NewProduction(context.Background(), ProjectID)
	if err != nil {
		t.Fatal(err)
	}

	claims := func() map[string]any {
		now := time.Now().Unix()
		return map[string]any{"aud": ProjectID, "iss": "https://securetoken.google.com/" + ProjectID, "sub": "real-owner", "iat": now - 10, "exp": now + 3600, "auth_time": now - 10, "firebase": map[string]any{"sign_in_provider": "password"}}
	}
	for _, kind := range []string{"valid", "wrong-audience", "wrong-issuer", "expired", "bad-signature", "malformed", "disabled", "revoked", "lookup-failure"} {
		t.Run(kind, func(t *testing.T) {
			c := claims()
			disabled = false
			validSince = "0"
			lookupFailure = false
			switch kind {
			case "wrong-audience":
				c["aud"] = "other"
			case "wrong-issuer":
				c["iss"] = "forged"
			case "expired":
				c["exp"] = time.Now().Unix() - 3600
			case "disabled":
				disabled = true
			case "revoked":
				validSince = "9999999999"
			case "lookup-failure":
				lookupFailure = true
			}
			credential := jwt(t, key, c)
			if kind == "bad-signature" {
				parts := strings.Split(credential, ".")
				sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
				sig[0] ^= 1
				parts[2] = base64.RawURLEncoding.EncodeToString(sig)
				credential = strings.Join(parts, ".")
			}
			if kind == "malformed" {
				credential = "not a token"
			}
			before := lookupCount
			identity, err := v.Verify(context.Background(), credential)
			if kind == "valid" {
				if err != nil || identity.UID != "real-owner" || lookupCount != before+1 {
					t.Fatalf("real SDK validation %v %+v", err, identity)
				}
			} else {
				want := publishing.ErrUnauthorized
				if kind == "lookup-failure" {
					want = publishing.ErrUnavailable
				}
				if !errors.Is(err, want) || identity.UID != "" {
					t.Fatalf("SDK %s got %v", kind, err)
				}
				if (kind == "wrong-audience" || kind == "wrong-issuer" || kind == "expired" || kind == "bad-signature" || kind == "malformed") && lookupCount != before {
					t.Fatal("invalid signature/claims triggered account lookup")
				}
			}
		})
	}
}

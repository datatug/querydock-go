package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"firebase.google.com/go/v4/auth"
	"github.com/datatug/querydock-go/internal/publishing"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Test-only construction is unavailable to production. The real SDK accepts
// unsigned demo tokens only against this explicitly owned test emulator.
func TestFirebaseEmulator(t *testing.T) {
	project := os.Getenv("QUERYDOCK_TEST_PROJECT")
	if project == "" {
		t.Skip("run scripts/sdk-emulators.py")
	}
	host := os.Getenv("FIREBASE_AUTH_EMULATOR_HOST")
	hostname, _, err := net.SplitHostPort(host)
	if err != nil || hostname != "127.0.0.1" || !strings.HasPrefix(project, "demo-querydock-") {
		t.Fatal("refusing unowned/non-demo auth target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := sdkClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	v := Verifier{client: client, project: project, getenv: emptyEnv}
	if _, err := NewProduction(ctx, ProjectID); !errors.Is(err, publishing.ErrUnavailable) {
		t.Fatal("production accepted emulator environment")
	}
	signup := func(input map[string]any) (string, string) {
		t.Helper()
		raw, _ := json.Marshal(input)
		req, err := http.NewRequestWithContext(ctx, "POST", "http://"+host+"/identitytoolkit.googleapis.com/v1/accounts:signUp?key=demo-only", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := response.Body.Close(); err != nil {
				t.Errorf("close auth response: %v", err)
			}
		}()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("demo signup status%d err%v", response.StatusCode, err)
		}
		var result struct{ IDToken, LocalID string }
		if err := json.Unmarshal(data, &result); err != nil || result.IDToken == "" || result.LocalID == "" {
			t.Fatal("invalid demo auth response")
		}
		return result.IDToken, result.LocalID
	}
	credential, uid := signup(map[string]any{"email": "owner@example.test", "password": "querydock-test-password", "returnSecureToken": true})
	identity, err := v.Verify(ctx, credential)
	if err != nil || identity.UID != uid || identity.Anonymous {
		t.Fatalf("actual SDK %+v %v", identity, err)
	}
	anonymous, _ := signup(map[string]any{"returnSecureToken": true})
	identity, err = v.Verify(ctx, anonymous)
	if err != nil || !identity.Anonymous {
		t.Fatalf("anonymous provider lost %+v %v", identity, err)
	}
	if _, err := v.Verify(ctx, "malformed"); !errors.Is(err, publishing.ErrUnauthorized) {
		t.Fatal("invalid token accepted")
	}
	concrete := client.(*auth.Client)
	if _, err := concrete.UpdateUser(ctx, uid, (&auth.UserToUpdate{}).Disabled(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, credential); !errors.Is(err, publishing.ErrUnauthorized) {
		t.Fatalf("disabled account accepted: %v", err)
	}
	if err := concrete.DeleteUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, credential); !errors.Is(err, publishing.ErrUnauthorized) {
		t.Fatalf("deleted account accepted: %v", err)
	}
}

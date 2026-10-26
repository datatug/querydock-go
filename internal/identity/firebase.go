// Package identity converts verified Firebase Admin SDK tokens into API authority.
package identity

import (
	"context"
	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"github.com/datatug/querydock-go/internal/publishing"
	"os"
)

const ProjectID = "sneat-eur3-1"

type tokenClient interface {
	VerifyIDTokenAndCheckRevoked(context.Context, string) (*auth.Token, error)
}
type Verifier struct {
	client  tokenClient
	project string
	getenv  func(string) string
}

var newApp = firebase.NewApp
var appAuth = (*firebase.App).Auth

func sdkClient(ctx context.Context, project string) (tokenClient, error) {
	app, err := newApp(ctx, &firebase.Config{ProjectID: project})
	if err != nil {
		return nil, publishing.ErrUnavailable
	}
	client, err := appAuth(app, ctx)
	if err != nil {
		return nil, publishing.ErrUnavailable
	}
	return client, nil
}

// NewProduction uses the explicit DataTug Firebase audience and rejects unsigned
// emulator tokens. Revocation/user-disabled checks require firebaseauth.users.get.
func NewProduction(ctx context.Context, project string) (*Verifier, error) {
	return newVerifier(ctx, project, os.Getenv, sdkClient)
}
func newVerifier(ctx context.Context, project string, getenv func(string) string, factory func(context.Context, string) (tokenClient, error)) (*Verifier, error) {
	if project != ProjectID || emulatorConfigured(getenv) {
		return nil, publishing.ErrUnavailable
	}
	client, err := factory(ctx, project)
	if err != nil || client == nil {
		return nil, publishing.ErrUnavailable
	}
	return &Verifier{client: client, project: project, getenv: getenv}, nil
}
func emulatorConfigured(getenv func(string) string) bool {
	return getenv("FIREBASE_AUTH_EMULATOR_HOST") != "" || getenv("FIRESTORE_EMULATOR_HOST") != ""
}
func (v *Verifier) Verify(ctx context.Context, credential string) (publishing.Identity, error) {
	if emulatorConfigured(v.getenv) {
		return publishing.Identity{}, publishing.ErrUnavailable
	}
	token, err := v.client.VerifyIDTokenAndCheckRevoked(ctx, credential)
	if err != nil {
		if auth.IsIDTokenInvalid(err) || auth.IsUserNotFound(err) {
			return publishing.Identity{}, publishing.ErrUnauthorized
		}
		return publishing.Identity{}, publishing.ErrUnavailable
	}
	if token == nil || token.UID == "" || token.UID != token.Subject || token.Audience != v.project || token.Issuer != "https://securetoken.google.com/"+v.project || token.Firebase.SignInProvider == "" {
		return publishing.Identity{}, publishing.ErrUnauthorized
	}
	return publishing.Identity{UID: token.UID, Anonymous: token.Firebase.SignInProvider == "anonymous"}, nil
}

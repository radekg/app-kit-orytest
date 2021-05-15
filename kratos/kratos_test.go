package kratos

import (
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"testing"
	"time"

	kratosAdmin "github.com/ory/kratos-client-go/client/admin"
	kratosPublic "github.com/ory/kratos-client-go/client/public"
	kratosModels "github.com/ory/kratos-client-go/models"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/mailslurper"
	"github.com/radekg/app-kit-orytest/postgres"
)

func TestKratosEmbeddedAPIRegistration(t *testing.T) {

	userEmail := "user@example.com"
	userPassword := time.Now().String()
	userInvalidPassword := time.Now().Add(time.Second).String()

	postgresCtx := postgres.SetupTestPostgres(t)
	mailslurperCtx := mailslurper.SetupTestMailslurper(t)
	kratosSelfService := DefaultKratosTestSelfService(t)
	defer kratosSelfService.Close()

	kratosCtx := SetupTestKratos(t, postgresCtx, mailslurperCtx, kratosSelfService)
	defer kratosCtx.Cleanup()

	okRes, err := kratosCtx.ClientPublic().
		InitializeSelfServiceRegistrationViaAPIFlow(kratosPublic.
			NewInitializeSelfServiceRegistrationViaAPIFlowParams())
	if err != nil {
		t.Fatalf("expected kratos self-service registration API request to be executed but received an error: '%v'", err)
	}

	t.Log(" ================> ", okRes.Payload.Active)

	method, ok := okRes.Payload.Methods["password"]
	if !ok {
		t.Fatal("Expected password method to be exist in methods but none found")
	}

	t.Log(*method.Config.Action)
	for _, field := range method.Config.Fields {
		t.Log(*field.Name, *field.Type, field.Value, field.Required)
	}

	registrationOkRes, err := kratosCtx.ClientPublic().
		CompleteSelfServiceRegistrationFlowWithPasswordMethod(kratosPublic.
			NewCompleteSelfServiceRegistrationFlowWithPasswordMethodParams().
			WithFlow(common.StringP(string(*okRes.Payload.ID))).
			WithPayload(map[string]interface{}{
				"password":         userPassword,
				"traits.email":     userEmail,
				"traits.firstName": "Registration",
				"traits.lastName":  "Test",
			}))
	if err != nil {
		t.Fatalf("unexpected error: '%v'", err)
	}

	createdIdentityID := registrationOkRes.Payload.Identity.ID

	// the verifiable addresses must contain an email address with status pending:
	hasPending := false
	for _, address := range registrationOkRes.Payload.Identity.VerifiableAddresses {
		if *address.Value == userEmail && string(*address.Via) == "email" && string(*address.Status) == "pending" {
			hasPending = true
			break
		}
	}
	if !hasPending {
		t.Fatalf("registered entity does not have an email address '%s' with status pending", userEmail)
	}

	verificationLink := ""
outsideEmailList:
	for {
		mail, err := mailslurperCtx.ListEmail()
		if err != nil {
			t.Fatalf("expected mail but received an error: '%v'", err)
		}
		if mail.TotalRecords == 0 {
			time.Sleep(time.Second)
		} else {
			for _, mailItem := range mail.MailItems {
				if common.StringSlicesEqual(t, mailItem.ToAddresses, []string{userEmail}) {
					// this is the email message we've been waiting for:
					re := regexp.MustCompile("\\<a.*\\>(.*)\\<\\/a\\>")
					match := re.FindStringSubmatch(mailItem.Body)
					if len(match) == 0 {
						t.Fatalf("verification email received but verification link could not be extracted, body: '%s'", mailItem.Body)
					}
					verificationLink = match[1]
					t.Log("verification link obtained from email: ", verificationLink)
					break outsideEmailList
				}
			}
		}
	}

	// verify the account, do not follow redirects, expect a redirect back to the
	// verification URL
	jar, jarErr := cookiejar.New(nil)
	if jarErr != nil {
		t.Fatalf("could not create a cookie jar for HTTP client, reason: '%v'", jarErr)
	}
	httpClient := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Jar: jar,
	}
	verificationResponse, err := httpClient.Get(verificationLink)
	if err != nil {
		t.Fatalf("expected verification GET request to be issued but received an error: '%v'", err)
	}

	if verificationResponse.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect after verification, received status: '%d', expected: '%d'",
			verificationResponse.StatusCode,
			http.StatusFound)
	}

	redirectLocation, err := verificationResponse.Location()
	if err != nil {
		t.Fatalf("expected redirect location but received an error: '%v'", err)
	}

	// Kratos is configured to redirect to login after verification.
	expectedRedirectLocation := kratosSelfService.LoginURL()
	if redirectLocation.String() != expectedRedirectLocation {
		t.Fatalf("received redirect location different than expected: '%s' vs '%s'",
			redirectLocation.String(),
			expectedRedirectLocation)
	}

	listIdentitiesOkRes, err := kratosCtx.ClientAdmin().ListIdentities(kratosAdmin.NewListIdentitiesParams().
		WithPage(common.Int64P(1)).
		WithPerPage(common.Int64P(100)))
	if err != nil {
		t.Fatalf("expected kratos admin list identities request to execute but received an error: '%v'", err)
	}

	// the verifiable addresses must now contain an email address with status completed:
	hasCompleted := false
outsideValidateIdentity:
	for _, identity := range listIdentitiesOkRes.Payload {
		if identity.ID == createdIdentityID {
			for _, address := range identity.VerifiableAddresses {
				if *address.Value == userEmail && string(*address.Via) == "email" && string(*address.Status) == "completed" {
					hasCompleted = true
					break outsideValidateIdentity
				}
			}
		}
	}

	if !hasCompleted {
		t.Fatalf("registered entity does not have an email address '%s' with status completed", userEmail)
	}

	// attempt login with correct password:
	loginResponseOk, err := kratosCtx.ClientPublic().
		InitializeSelfServiceLoginViaAPIFlow(kratosPublic.
			NewInitializeSelfServiceLoginViaAPIFlowParams())

	if err != nil {
		t.Fatalf("unexpected error while logging in: '%v'", err)
	}

	loginMethod, ok := loginResponseOk.Payload.Methods["password"]
	if !ok {
		t.Fatal("Expected password method to be exist in methods but none found")
	}

	t.Log(*loginMethod.Config.Action)
	for _, field := range loginMethod.Config.Fields {
		t.Log(*field.Name, *field.Type, field.Value, field.Required)
	}

	loginCompleteOkRes, err := kratosCtx.ClientPublic().
		CompleteSelfServiceLoginFlowWithPasswordMethod(kratosPublic.
			NewCompleteSelfServiceLoginFlowWithPasswordMethodParams().
			WithFlow(string(*loginResponseOk.Payload.ID)).
			WithBody(&kratosModels.CompleteSelfServiceLoginFlowWithPasswordMethod{
				Identifier: userEmail,
				Password:   userPassword,
			}))
	if err != nil {
		t.Fatalf("unexpected error: '%v'", err)
	}

	if loginCompleteOkRes.Payload.Session == nil {
		t.Fatalf("Expected session after login but got none, login response: '%s'", loginCompleteOkRes.Error())
	}

	// attempt login with incorrect password:
	loginResponseOk, err = kratosCtx.ClientPublic().
		InitializeSelfServiceLoginViaAPIFlow(kratosPublic.
			NewInitializeSelfServiceLoginViaAPIFlowParams())

	if err != nil {
		t.Fatalf("unexpected error while logging in: '%v'", err)
	}

	_, loginErr := kratosCtx.ClientPublic().
		CompleteSelfServiceLoginFlowWithPasswordMethod(kratosPublic.
			NewCompleteSelfServiceLoginFlowWithPasswordMethodParams().
			WithFlow(string(*loginResponseOk.Payload.ID)).
			WithBody(&kratosModels.CompleteSelfServiceLoginFlowWithPasswordMethod{
				Identifier: userEmail,
				Password:   userInvalidPassword,
			}))
	if loginErr == nil {
		t.Fatalf("expected an error but received none")
	}

	// attempt to change settings:

	loggedInClientWithHeader := &http.Client{
		Transport: &TransportWithHeader{
			RoundTripper: http.DefaultTransport,
			h: http.Header{
				"Authorization": {"Bearer " + *loginCompleteOkRes.Payload.SessionToken},
			},
		},
	}

	settingsInitFlowOk, err := kratosCtx.ClientPublic().
		InitializeSelfServiceSettingsViaAPIFlow(kratosPublic.
			NewInitializeSelfServiceSettingsViaAPIFlowParams().
			WithHTTPClient(loggedInClientWithHeader), nil)
	if err != nil {
		t.Fatalf("unexpected error: '%v'", err)
	}

	if _, ok := settingsInitFlowOk.Payload.Methods["password"]; !ok {
		t.Fatalf("expected password method available in settings flow methods")
	}
	if _, ok := settingsInitFlowOk.Payload.Methods["profile"]; !ok {
		t.Fatalf("expected profile method available in settings flow methods")
	}

	_, updatePasswordErr := kratosCtx.ClientPublic().
		CompleteSelfServiceSettingsFlowWithPasswordMethod(kratosPublic.
			NewCompleteSelfServiceSettingsFlowWithPasswordMethodParams().
			WithHTTPClient(loggedInClientWithHeader).
			WithFlow(common.StringP(string(*settingsInitFlowOk.Payload.ID))).
			WithBody(&kratosModels.CompleteSelfServiceSettingsFlowWithPasswordMethod{
				Password: common.StringP(userInvalidPassword),
			}), nil)
	if updatePasswordErr != nil {
		t.Fatalf("unexpected error: '%v'", updatePasswordErr)
	}

	// attempt login with incorrect password - this time it should work:
	loginResponseOk, err = kratosCtx.ClientPublic().
		InitializeSelfServiceLoginViaAPIFlow(kratosPublic.
			NewInitializeSelfServiceLoginViaAPIFlowParams())

	if err != nil {
		t.Fatalf("unexpected error while logging in: '%v'", err)
	}

	loginCompleteOkRes, err = kratosCtx.ClientPublic().
		CompleteSelfServiceLoginFlowWithPasswordMethod(kratosPublic.
			NewCompleteSelfServiceLoginFlowWithPasswordMethodParams().
			WithFlow(string(*loginResponseOk.Payload.ID)).
			WithBody(&kratosModels.CompleteSelfServiceLoginFlowWithPasswordMethod{
				Identifier: userEmail,
				Password:   userInvalidPassword,
			}))
	if err != nil {
		t.Fatalf("unexpected error: '%v'", err)
	}

	if loginCompleteOkRes.Payload.Session == nil {
		t.Fatalf("Expected session after login but got none, login response: '%s'", loginCompleteOkRes.Error())
	}
}

type TransportWithHeader struct {
	http.RoundTripper
	h http.Header
}

func (ct *TransportWithHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	for k := range ct.h {
		req.Header.Set(k, ct.h.Get(k))
	}
	return ct.RoundTripper.RoundTrip(req)
}

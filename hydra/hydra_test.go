package hydra

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"

	"github.com/radekg/app-kit-tokens/webfinger"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/postgres"
)

func TestHydraEmbedded(t *testing.T) {

	postgresCtx := postgres.SetupTestPostgres(t)
	hydraCtx := SetupTestHydra(t, postgresCtx)
	defer hydraCtx.Cleanup()

	// here, we can try setting up a client:
	hydraClientPayload := &CreateClientArguments{
		Audience:                []string{"integration-test"},
		ClientID:                common.StringP("integration-test"),
		ClientName:              common.StringP("integration-test"),
		ClientSecret:            common.StringP("integration-test-secret"),
		GrantTypes:              []string{"authorization_code", "client_credentials", "refresh_token"},
		RedirectURIs:            []string{"http://127.0.0.1:5555/callback"},
		ResponseTypes:           []string{"code", "id_token", "token"},
		Scope:                   common.StringP("openid offline"),
		TokenEndpointAuthMethod: common.StringP("client_secret_post"),
	}

	hydraClientPayloadBytes, jsonErr := json.Marshal(hydraClientPayload)
	if jsonErr != nil {
		t.Fatalf("expected hydra client create params to serialize to JSON but received an error: '%v'", jsonErr)
	}

	request, requestErr := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://localhost:%s/clients", hydraCtx.AdminPort()),
		bytes.NewReader(hydraClientPayloadBytes))
	if requestErr != nil {
		t.Fatalf("expected hydra client POST create request to be constructed but received an error: '%v'", requestErr)
	}

	httpClient := &http.Client{}
	response, responseErr := httpClient.Do(request)
	if responseErr != nil {
		t.Fatalf("expected hydra client POST create request to be executed but received an error: '%v'", responseErr)
	}

	responseBytes, readErr := ioutil.ReadAll(response.Body)
	defer response.Body.Close()
	if readErr != nil {
		t.Fatalf("failed reading response body data: '%v'", readErr)
	}

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected hydra client POST to return status 201 but received status: '%d', body: '%s'", response.StatusCode, string(responseBytes))
	}

	t.Logf("hydra client created with status: '%d', body: '%s'", response.StatusCode, string(responseBytes))

	// it should be possible to resolve a webfinger of the embedded hydra:
	//openIDConfig, resolveErr := webfinger.ResolveOpenIDConfiguration(fmt.Sprintf("http://localhost:%s", hydraCtx.PublicPort()))
	openIDConfig, resolveErr := webfinger.ResolveOpenIDConfiguration(fmt.Sprintf("http://localhost:%s", hydraCtx.PublicPort()))
	if resolveErr != nil {
		t.Fatalf("expected webfinger to resolve in embedded hydra but received an error: '%v'", resolveErr)
	}

	_, resolveJWKSErr := openIDConfig.ResolveJWKS()
	if resolveJWKSErr != nil {
		t.Fatalf("expected JWKS to resolve in embedded hydra but received an error: '%v'", resolveJWKSErr)
	}
}

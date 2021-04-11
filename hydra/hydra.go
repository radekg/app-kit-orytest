package hydra

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"

	"gopkg.in/yaml.v2"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/postgres"
)

const (
	// DefaultHydraImageName specifies the Hydra docker image name to use in tests.
	DefaultHydraImageName = "oryd/hydra"
	// DefaultHydraEnvVarImageName is the environment variable name for default Hydra docker image name.
	DefaultHydraEnvVarImageName = "TEST_ORY_HYDRA_IMAGE_NAME"
	// DefaultHydraImageVersion specifies the Hydra docker image version to use in tests.
	DefaultHydraImageVersion = "v1.10.1"
	// DefaultHydraEnvVarImageVersion is the environment variable name for default Hydra docker image version.
	DefaultHydraEnvVarImageVersion = "TEST_ORY_HYDRA_IMAGE_VERSION"
)

// TestEnvContext represents a test hydra environment context.
type TestEnvContext interface {
	Pool() *dockertest.Pool
	PublicPort() string
	AdminPort() string
	Cleanup()
}

// SetupTestHydra sets up hydra environment for tests.
func SetupTestHydra(t *testing.T, postgresCtx postgres.TestEnvContext) TestEnvContext {

	// after every successful step, store a cleanup function here:
	closables := []func(){
		postgresCtx.Cleanup,
	}

	// used in case of a failure during setup:
	closeClosables := func(closables []func()) {
		for _, closable := range closables {
			defer closable()
		}
	}

	// close all reasources in reverse order:
	prependClosable := func(closable func(), closables []func()) []func() {
		return append([]func(){closable}, closables...)
	}

	// create a temp file for the configuration:
	hydraConfig, tempFileErr := ioutil.TempFile("", "hydra-config")
	if tempFileErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Hydra configuration file to be created but received an error: '%v'", tempFileErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: removing hydra config")
		os.Remove(hydraConfig.Name())
	}, closables)

	// we need two random ports:
	publicListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating public random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing public random port listener, if not closed yet")
		publicListener.Cleanup()
	}, closables)
	adminListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating admin random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing admin random port listener, if not closed yet")
		adminListener.Cleanup()
	}, closables)

	if err := publicListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from public random port listener: '%v'", err)
	}
	if err := adminListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from admin random port listener: '%v'", err)
	}

	fetchedHydraRandomPublicPort, _ := publicListener.DiscoveredPort()
	fetchedHydraRandomAdminPort, _ := adminListener.DiscoveredPort()

	// write test configuration to the temp file:
	// hydra config requires the port from the public listener:

	hydraSerializedConfigBytes, yamlErr := yaml.Marshal(hydraTestConfigFunc(t, fetchedHydraRandomPublicPort))
	if yamlErr != nil {
		closeClosables(closables)
		t.Fatalf("failed serializing Hydra config to YAML: '%v'", err)
	}

	if writeErr := ioutil.WriteFile(hydraConfig.Name(), hydraSerializedConfigBytes, 0777); writeErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Hydra configuration file to be written but received an error: '%v'", writeErr)
	}

	t.Log("Hydra config written...", hydraConfig.Name())

	// create new pool using the default Docker endpoint:
	pool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		closeClosables(closables)
		t.Fatalf("expected docker pool to come up but received: '%v'", poolErr)
	}

	migrateOptions := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultHydraEnvVarImageName, DefaultHydraImageName),
		Tag:        common.GetEnvOrDefault(DefaultHydraEnvVarImageVersion, DefaultHydraImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/hydra/hydra.yml", hydraConfig.Name()),
		},
		Cmd: []string{"migrate", "-c", "/etc/config/hydra/hydra.yml", "sql", "-e", "--yes"},
		Env: []string{
			"LOG_LEVEL=trace",
			fmt.Sprintf("DSN=postgres://%s:%s@%s:%s/%s?sslmode=disable&max_conns=20&max_idle_conns=4",
				postgresCtx.PostgresUser(),
				postgresCtx.PostgresPass(),
				postgresCtx.ContainerIP(),
				postgresCtx.ContainerPrivatePort(),
				postgresCtx.PostgresDatabase()),
		},
	}
	hydraMigrate, hydraErr := pool.RunWithOptions(migrateOptions, func(config *dc.HostConfig) {
		// Hydra migrate does not automatically retry failed migrations
		config.RestartPolicy.Name = "on-failure"
	})
	if hydraErr != nil {
		closeClosables(closables)
		t.Fatalf("expected hydra migrate to start but received: '%v'", hydraErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: closing hydra migrate")
		hydraMigrate.Close()
		pool.Purge(hydraMigrate)
	}, closables)

	if err := common.WaitForContainerExit0(t, pool, hydraMigrate.Container.ID); err != nil {
		closeClosables(closables)
		t.Fatalf("error while waiting for container to finish with exist status 0: '%v'", err)
	}

	// create test hydra run options:
	options := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultHydraEnvVarImageName, DefaultHydraImageName),
		Tag:        common.GetEnvOrDefault(DefaultHydraEnvVarImageVersion, DefaultHydraImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/hydra/hydra.yml", hydraConfig.Name()),
		},
		Cmd: []string{"serve", "-c", "/etc/config/hydra/hydra.yml", "all", "--dangerous-force-http"},
		ExposedPorts: []string{
			fmt.Sprintf("%s/tcp", fetchedHydraRandomPublicPort),
			fmt.Sprintf("%s/tcp", fetchedHydraRandomAdminPort)},
		PortBindings: map[dc.Port][]dc.PortBinding{
			dc.Port(fmt.Sprintf("%s/tcp", fetchedHydraRandomPublicPort)): {{HostIP: "0.0.0.0", HostPort: fetchedHydraRandomPublicPort}},
			dc.Port(fmt.Sprintf("%s/tcp", fetchedHydraRandomAdminPort)):  {{HostIP: "0.0.0.0", HostPort: fetchedHydraRandomAdminPort}},
		},
		Env: []string{
			fmt.Sprintf("SERVE_PUBLIC_PORT=%s", fetchedHydraRandomPublicPort),
			"SERVE_PUBLIC_HOST=0.0.0.0",
			fmt.Sprintf("SERVE_ADMIN_PORT=%s", fetchedHydraRandomAdminPort),
			"SERVE_ADMIN_HOST=0.0.0.0",
			"LOG_LEVEL=trace",
			fmt.Sprintf("DSN=postgres://%s:%s@%s:%s/%s?sslmode=disable&max_conns=20&max_idle_conns=4",
				postgresCtx.PostgresUser(),
				postgresCtx.PostgresPass(),
				postgresCtx.ContainerIP(),
				postgresCtx.ContainerPrivatePort(),
				postgresCtx.PostgresDatabase()),
		},
	}

	// make sure the random ports are free before we request the container to start:
	adminListener.Cleanup()
	publicListener.Cleanup()

	// start the container:
	hydra, hydraErr := pool.RunWithOptions(options, func(config *dc.HostConfig) {})
	if hydraErr != nil {
		closeClosables(closables)
		t.Fatalf("expected hydra to start but received: '%v'", hydraErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing hydra")
		hydra.Close()
		pool.Purge(hydra)
	}, closables)

	benchStart := time.Now()

	chanHydraRESTAPISuccess := make(chan struct{}, 1)
	chanHydraRESTAPIError := make(chan error, 1)

	hydraAdminPort := hydra.GetPort(fmt.Sprintf("%s/tcp", fetchedHydraRandomAdminPort))
	hydraPublicPort := hydra.GetPort(fmt.Sprintf("%s/tcp", fetchedHydraRandomPublicPort))

	t.Logf("Hydra started with container ID '%s', waiting for the REST API to start replying...", hydra.Container.ID)

	go func() {
		poolRetryErr := pool.Retry(func() error {
			request, requestErr := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%s/health/ready", hydraAdminPort), nil)
			if requestErr != nil {
				t.Logf("expected /clients request to be constructed but received: '%v'", requestErr)
				return requestErr
			}
			httpClient := &http.Client{}
			response, responseErr := httpClient.Do(request)
			if responseErr != nil {
				t.Logf("expected /health/ready to reply but received: '%v'", responseErr)
				return responseErr
			}
			responseBytes, responseReadErr := ioutil.ReadAll(response.Body)
			if responseErr != nil {
				t.Logf("failed reading response body: '%v'", responseReadErr)
				return responseErr
			}
			defer response.Body.Close()
			t.Log("hydra /health/ready replied with status: ", response.StatusCode, string(responseBytes))
			if response.StatusCode == http.StatusOK {
				t.Logf("hydra /health/ready replied with status OK, body '%s'", string(responseBytes))
				return nil
			}
			t.Logf("hydra /health/ready replied with status other than OK: '%d', body: '%s'", response.StatusCode, string(responseBytes))
			return fmt.Errorf("hydra rest status code other than OK: %d", response.StatusCode)
		})
		if poolRetryErr == nil {
			close(chanHydraRESTAPISuccess)
			return
		}
		chanHydraRESTAPIError <- poolRetryErr
	}()

	select {
	case <-chanHydraRESTAPISuccess:
		t.Logf("hydra REST API replied after: %s", time.Now().Sub(benchStart).String())
	case receivedError := <-chanHydraRESTAPIError:
		closeClosables(closables)
		t.Fatalf("hydra REST API wait finished with error: '%v'", receivedError)
	case <-time.After(time.Second * 30):
		closeClosables(closables)
		t.Fatalf("hydra REST API did not start communicating within timeout")
	}

	// return the context:
	return &testHydraContext{
		AdminPortValue: hydraAdminPort,
		CleanupFuncValue: func() {
			for _, closable := range closables {
				closable()
			}
		},
		PoolValue:       pool,
		PublicPortValue: hydraPublicPort,
	}

}

// -- default test context implementation:

type testHydraContext struct {
	AdminPortValue   string
	CleanupFuncValue func()
	PoolValue        *dockertest.Pool
	PublicPortValue  string
}

func (ctx *testHydraContext) AdminPort() string {
	return ctx.AdminPortValue
}

func (ctx *testHydraContext) Cleanup() {
	ctx.CleanupFuncValue()
}

func (ctx *testHydraContext) Pool() *dockertest.Pool {
	return ctx.PoolValue
}

func (ctx *testHydraContext) PublicPort() string {
	return ctx.PublicPortValue
}

// -- Hydra models:

// CreateClientArguments defines arguments for creating new Hydra OIDC client.
type CreateClientArguments struct {
	AllowedCorsOrigins                []string               `json:"allowed_cors_origins,omitempty"`
	Audience                          []string               `json:"audience,omitempty"`
	BackchannelLogoutSessionRequired  *bool                  `json:"backchannel_logout_session_required,omitempty"`
	BackchannelLogoutURI              *string                `json:"backchannel_logout_uri,omitempty"`
	ClientID                          *string                `json:"client_id,omitempty"`
	ClientName                        *string                `json:"client_name,omitempty"`
	ClientSecret                      *string                `json:"client_secret,omitempty"`
	ClientSecretExpireAt              *int64                 `json:"client_secret_expires_at,omitempty"`
	ClientURI                         *string                `json:"client_uri,omitempty"`
	Contacts                          []string               `json:"contacts,omitempty"`
	CreatedAt                         *string                `json:"created_at,omitempty"`
	FrontchannelLogoutSessionRequired *bool                  `json:"frontchannel_logout_session_required,omitempty"`
	FrontchannelLogoutURI             *string                `json:"frontchannel_logout_uri,omitempty"`
	GrantTypes                        []string               `json:"grant_types,omitempty"`
	JWKS                              map[string]interface{} `json:"jwks,omitempty"`
	JWKSURI                           *string                `json:"jwks_uri,omitempty"`
	LogoURI                           *string                `json:"logo_uri,omitempty"`
	Metadata                          map[string]interface{} `json:"metadata,omitempty"`
	Owner                             *string                `json:"owner,omitempty"`
	PolicyURI                         *string                `json:"policy_uri,omitempty"`
	PostLogoutRedirectURIs            []string               `json:"post_logout_redirect_uris,omitempty"`
	RedirectURIs                      []string               `json:"redirect_uris,omitempty"`
	RequestObjectSigningAlg           *string                `json:"request_object_signing_alg,omitempty"`
	RequestURIs                       []string               `json:"request_uris,omitempty"`
	ResponseTypes                     []string               `json:"response_types,omitempty"`
	Scope                             *string                `json:"scope,omitempty"`
	SectorIdentifierURI               *string                `json:"sector_identifier_uri,omitempty"`
	SubjectType                       *string                `json:"subject_type,omitempty"`
	TokenEndpointAuthMethod           *string                `json:"token_endpoint_auth_method,omitempty"`
	TokenEndpointAuthSigningAlg       *string                `json:"token_endpoint_auth_signing_alg,omitempty"`
	TOSURI                            *string                `json:"tos_uri,omitempty"`
	UpdatedAt                         *string                `json:"updated_at,omitempty"`
	UserinfoSignedResponseAlg         *string                `json:"userinfo_signed_response_alg,omitempty"`
}

// -- log writer

type myIOWriter struct {
	t    *testing.T
	mode string
}

func (w *myIOWriter) Write(p []byte) (n int, err error) {
	w.t.Log(w.mode, ": ", string(p))
	return len(p), nil
}

func hydraTestConfigFunc(t *testing.T, port string) map[string]interface{} {
	return map[string]interface{}{
		"serve": map[string]interface{}{
			"cookies": map[string]interface{}{
				"same_site_mode": "Lax",
			},
		},
		"urls": map[string]interface{}{
			"self": map[string]interface{}{
				"issuer": fmt.Sprintf("http://127.0.0.1:%s", port),
			},
			"consent": "http://127.0.0.1:3000/consent",
			"login":   "http://127.0.0.1:3000/login",
			"logout":  "http://127.0.0.1:3000/logout",
		},
		"secrets": map[string]interface{}{
			"system": []string{"youReallyNeedToChangeThis"},
		},
		"strategies": map[string]interface{}{
			"access_token": "jwt",
		},
		"oidc": map[string]interface{}{
			"subject_identifiers": map[string]interface{}{
				"supported_types": []string{"pairwise", "public"},
				"pairwise": map[string]interface{}{
					"salt": "youReallyNeedToChangeThis",
				},
			},
		},
	}
}

package kratos

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"
	"gopkg.in/yaml.v2"

	kratosClient "github.com/ory/kratos-client-go/client"
	kratosAdmin "github.com/ory/kratos-client-go/client/admin"
	kratosPublic "github.com/ory/kratos-client-go/client/public"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/mailslurper"
	"github.com/radekg/app-kit-orytest/postgres"
)

const (
	// DefaultKratosImageName specifies the Kratos docker image name to use in tests.
	DefaultKratosImageName = "oryd/kratos"
	// DefaultKratosEnvVarImageName is the environment variable name for default Kratos docker image name.
	DefaultKratosEnvVarImageName = "TEST_ORY_KRATOS_IMAGE_NAME"
	// DefaultKratosImageVersion specifies the Kratos docker image version to use in tests.
	DefaultKratosImageVersion = "v0.5.5-alpha.1"
	// DefaultKratosEnvVarImageVersion is the environment variable name for default Kratos docker image version.
	DefaultKratosEnvVarImageVersion = "TEST_ORY_KRATOS_IMAGE_VERSION"
)

// TestEnvContext represents a test Kratos environment context.
type TestEnvContext interface {
	AdminPort() string
	Cleanup()
	ClientAdmin() kratosAdmin.ClientService
	ClientPublic() kratosPublic.ClientService
	Pool() *dockertest.Pool
	PublicPort() string
}

// SetupTestKratos sets up Kratos environment for tests.
func SetupTestKratos(t *testing.T,
	postgresCtx postgres.TestEnvContext,
	mailslurperCtx mailslurper.TestEnvContext,
	selfService TestSelfServiceConfiguration) TestEnvContext {

	cleanupContainers := true

	// after every successful step, store a cleanup function here:
	closables := []func(){
		postgresCtx.Cleanup,
		mailslurperCtx.Cleanup,
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

	// public Kratos port:
	kratosAdminListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating kratos admin random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing kratos admin random port listener, if not closed yet")
		kratosAdminListener.Cleanup()
	}, closables)

	// public Kratos port:
	kratosPublicListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating kratos public random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing kratos public random port listener, if not closed yet")
		kratosPublicListener.Cleanup()
	}, closables)

	if err := kratosAdminListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from kratos admin random port listener: '%v'", err)
	}
	if err := kratosPublicListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from kratos public random port listener: '%v'", err)
	}

	fetchedKratosAdminRandomPort, _ := kratosAdminListener.DiscoveredPort()
	fetchedKratosPublicRandomPort, _ := kratosPublicListener.DiscoveredPort()

	// create new pool using the default Docker endpoint:
	pool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		closeClosables(closables)
		t.Fatalf("expected docker pool to come up but received: '%v'", poolErr)
	}

	// create a temp file for the identity:
	kratosIdentity, tempFileErr := ioutil.TempFile("", "kratos-identity")
	if tempFileErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Kratos identity file to be created but received an error: '%v'", tempFileErr)
	}

	if writeErr := ioutil.WriteFile(kratosIdentity.Name(), []byte(kratosTestIdentity), 0777); writeErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Kratos configuration file to be written but received an error: '%v'", writeErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: removing kratos identity")
		os.Remove(kratosIdentity.Name())
	}, closables)

	t.Log("Kratos identity written...", kratosIdentity.Name())

	// create a temp file for the configuration:
	kratosConfig, tempFileErr := ioutil.TempFile("", "kratos-config")
	if tempFileErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Kratos configuration file to be created but received an error: '%v'", tempFileErr)
	}

	kratosTestConfig := kratosTestConfigFunc(t, fetchedKratosAdminRandomPort, fetchedKratosPublicRandomPort,
		mailslurperCtx,
		selfService)
	kratosSerializedConfigBytes, yamlErr := yaml.Marshal(kratosTestConfig)
	if yamlErr != nil {
		closeClosables(closables)
		t.Fatalf("failed serializing Kratos config to YAML: '%v'", err)
	}

	if writeErr := ioutil.WriteFile(kratosConfig.Name(), kratosSerializedConfigBytes, 0777); writeErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Kratos configuration file to be written but received an error: '%v'", writeErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: removing kratos config")
		os.Remove(kratosConfig.Name())
	}, closables)

	t.Log("Kratos config written...", kratosConfig.Name())

	// Now, we need to run kratos migrate, postgres is starting but kratos retries, we need to wait for it...
	migrateOptions := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultKratosEnvVarImageName, DefaultKratosImageName),
		Tag:        common.GetEnvOrDefault(DefaultKratosEnvVarImageVersion, DefaultKratosImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/kratos/identity.schema.json", kratosIdentity.Name()),
			fmt.Sprintf("%s:/etc/config/kratos/kratos.yml", kratosConfig.Name()),
		},
		Cmd: []string{"migrate", "-c", "/etc/config/kratos/kratos.yml", "sql", "-e", "--yes"},
		Env: []string{
			fmt.Sprintf("DSN=postgres://%s:%s@%s:%s/%s?sslmode=disable&max_conns=20&max_idle_conns=4",
				postgresCtx.PostgresUser(),
				postgresCtx.PostgresPass(),
				postgresCtx.ContainerIP(),
				postgresCtx.ContainerPrivatePort(),
				postgresCtx.PostgresDatabase()),
		},
	}
	kratosMigrate, kratosErr := pool.RunWithOptions(migrateOptions, func(config *dc.HostConfig) {
		// Keto migrate retries automatically but for compatibility, we make sure it does...
		config.RestartPolicy.Name = "on-failure"
	})
	if kratosErr != nil {
		closeClosables(closables)
		t.Fatalf("expected kratos migrate to start but received: '%v'", kratosErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: closing kratos migrate")
		kratosMigrate.Close()
		pool.Purge(kratosMigrate)
	}, closables)

	if err := common.WaitForContainerExit0(t, pool, kratosMigrate.Container.ID); err != nil {
		closeClosables(closables)
		t.Fatalf("error while waiting for container to finish with exist status 0: '%v'", err)
	}

	// create test kratos run options:
	options := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault("TEST_ORY_KRATOS_IMAGE_NAME", DefaultKratosImageName),
		Tag:        common.GetEnvOrDefault("TEST_ORY_KRATOS_IMAGE_NAME", DefaultKratosImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/kratos/identity.schema.json", kratosIdentity.Name()),
			fmt.Sprintf("%s:/etc/config/kratos/kratos.yml", kratosConfig.Name()),
		},
		Cmd: []string{"serve", "-c", "/etc/config/kratos/kratos.yml", "--dev"},
		ExposedPorts: []string{
			fmt.Sprintf("%s/tcp", fetchedKratosAdminRandomPort),
			fmt.Sprintf("%s/tcp", fetchedKratosPublicRandomPort)},
		PortBindings: map[dc.Port][]dc.PortBinding{
			dc.Port(fmt.Sprintf("%s/tcp", fetchedKratosAdminRandomPort)):  {{HostIP: "0.0.0.0", HostPort: fetchedKratosAdminRandomPort}},
			dc.Port(fmt.Sprintf("%s/tcp", fetchedKratosPublicRandomPort)): {{HostIP: "0.0.0.0", HostPort: fetchedKratosPublicRandomPort}},
		},
		Env: []string{
			fmt.Sprintf("SERVE_ADMIN_PORT=%s", fetchedKratosAdminRandomPort),
			fmt.Sprintf("SERVE_PUBLIC_PORT=%s", fetchedKratosPublicRandomPort),
			fmt.Sprintf("DSN=postgres://%s:%s@%s:%s/%s?sslmode=disable&max_conns=20&max_idle_conns=4",
				postgresCtx.PostgresUser(),
				postgresCtx.PostgresPass(),
				postgresCtx.ContainerIP(),
				postgresCtx.ContainerPrivatePort(),
				postgresCtx.PostgresDatabase()),
		},
	}

	// make sure the random kratos port is free before we request the container to start:
	kratosAdminListener.Cleanup()
	kratosPublicListener.Cleanup()

	// start the container:
	kratos, kratosErr := pool.RunWithOptions(options, func(config *dc.HostConfig) {
		config.RestartPolicy.Name = "on-failure"
	})
	if kratosErr != nil {
		closeClosables(closables)
		t.Fatalf("expected kratos to start but received: '%v'", kratosErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing kratos")
		if cleanupContainers {
			kratos.Close()
			pool.Purge(kratos)
		}
	}, closables)

	benchStart := time.Now()
	chanKratosRESTAPISuccess := make(chan struct{}, 1)
	chanKratosRESTAPIError := make(chan error, 1)

	kratosAdminPort := kratos.GetPort(fmt.Sprintf("%s/tcp", fetchedKratosAdminRandomPort))
	kratosPublicPort := kratos.GetPort(fmt.Sprintf("%s/tcp", fetchedKratosPublicRandomPort))

	t.Logf("Kratos started with container ID '%s', waiting for the REST API to start replying...", kratos.Container.ID)

	go func() {
		poolRetryErr := pool.Retry(func() error {
			request, requestErr := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%s/health/ready", kratosPublicPort), nil)
			if requestErr != nil {
				t.Logf("expected /health/ready request to be constructed but received: '%v'", requestErr)
				return requestErr
			}
			httpClient := &http.Client{}
			response, responseErr := httpClient.Do(request)
			if responseErr != nil {
				t.Logf("expected /health/ready to reply but received: '%v'", responseErr)
				return responseErr
			}
			defer response.Body.Close()
			t.Log("kratos /health/ready replied with status: ", response.StatusCode)
			if response.StatusCode == http.StatusOK {
				t.Logf("kratos /health/ready replied with status OK")
				return nil
			}
			t.Logf("kratos /health/ready replied with status other than OK: '%d'", response.StatusCode)
			return fmt.Errorf("kratos rest status code other than OK: %d", response.StatusCode)
		})
		if poolRetryErr == nil {
			close(chanKratosRESTAPISuccess)
			return
		}
		chanKratosRESTAPIError <- poolRetryErr
	}()

	select {
	case <-chanKratosRESTAPISuccess:
		t.Logf("kratos REST API replied after: %s", time.Now().Sub(benchStart).String())
	case receivedError := <-chanKratosRESTAPIError:
		closeClosables(closables)
		t.Fatalf("kratos REST API wait finished with error: '%v'", receivedError)
	case <-time.After(time.Second * 30):
		closeClosables(closables)
		t.Fatalf("kratos REST API did not start communicating within timeout")
	}

	return &testKratosContext{
		AdminPortValue: kratosAdminPort,
		CleanupFuncValue: func() {
			for _, closable := range closables {
				closable()
			}
		},
		ClientAdminValue: kratosClient.NewHTTPClientWithConfig(nil,
			kratosClient.
				DefaultTransportConfig().
				WithSchemes([]string{"http"}).
				WithHost(fmt.Sprintf("localhost:%s", kratosAdminPort))),
		ClientPublicValue: kratosClient.NewHTTPClientWithConfig(nil,
			kratosClient.
				DefaultTransportConfig().
				WithSchemes([]string{"http"}).
				WithHost(fmt.Sprintf("localhost:%s", kratosPublicPort))),
		PoolValue:       pool,
		PublicPortValue: kratosPublicPort,
	}

}

type testKratosContext struct {
	AdminPortValue    string
	CleanupFuncValue  func()
	ClientAdminValue  *kratosClient.OryKratos
	ClientPublicValue *kratosClient.OryKratos
	PoolValue         *dockertest.Pool
	PublicPortValue   string
}

func (ctx *testKratosContext) AdminPort() string {
	return ctx.AdminPortValue
}

func (ctx *testKratosContext) Cleanup() {
	ctx.CleanupFuncValue()
}

func (ctx *testKratosContext) ClientAdmin() kratosAdmin.ClientService {
	return ctx.ClientAdminValue.Admin
}

func (ctx *testKratosContext) ClientPublic() kratosPublic.ClientService {
	return ctx.ClientPublicValue.Public
}

func (ctx *testKratosContext) Pool() *dockertest.Pool {
	return ctx.PoolValue
}

func (ctx *testKratosContext) PublicPort() string {
	return ctx.PublicPortValue
}

func kratosTestConfigFunc(t *testing.T, adminPort, publicPort string,
	mailslurperCtx mailslurper.TestEnvContext,
	selfService TestSelfServiceConfiguration) map[string]interface{} {

	publicPortInt, _ := strconv.Atoi(publicPort)

	return map[string]interface{}{
		"serve": map[string]interface{}{
			"public": map[string]interface{}{
				"base_url": func() string {
					if selfService.BasePublicURL() == "" {
						return fmt.Sprintf("http://127.0.0.1:%s/", publicPort)
					}
					return selfService.BasePublicURL()
				}(),
				"cors": map[string]interface{}{
					"enabled": true,
				},
				"host": "0.0.0.0",
				"port": publicPortInt,
			},
			"admin": map[string]interface{}{
				"base_url": fmt.Sprintf("http://127.0.0.1:%s/", adminPort),
			},
		},
		"log": map[string]interface{}{
			"level":                 "trace",
			"format":                "text",
			"leak_sensitive_values": true,
		},
		"secrets": map[string]interface{}{
			"cookie": []string{"PLEASE-CHANGE-ME-I-AM-VERY-INSECURE"},
		},
		"hashers": map[string]interface{}{
			"argon2": map[string]interface{}{
				"parallelism": 1,
				"memory":      131072,
				"iterations":  2,
				"salt_length": 16,
				"key_length":  16,
			},
		},
		"identity": map[string]interface{}{
			"default_schema_url": "file:///etc/config/kratos/identity.schema.json",
		},
		"courier": map[string]interface{}{
			"smtp": map[string]interface{}{
				"connection_uri": fmt.Sprintf("smtps://test:test@%s:%s/?skip_ssl_verify=true",
					mailslurperCtx.ContainerIP(),
					mailslurperCtx.ContainerSMTPPort()),
			},
		},
		"selfservice": map[string]interface{}{
			"default_browser_return_url": selfService.DefaultBrowserReturnURL(),
			"whitelisted_return_urls":    selfService.WhitelistedReturnURLs(),
			"methods": map[string]interface{}{
				"password": map[string]interface{}{
					"enabled": true,
				},
			},
			"flows": map[string]interface{}{
				"error": map[string]interface{}{
					"ui_url": selfService.ErrorURL(),
				},
				"login": map[string]interface{}{
					"lifespan": "10m",
					"ui_url":   selfService.LoginURL(),
				},
				"logout": map[string]interface{}{
					"after": map[string]interface{}{
						"default_browser_return_url": selfService.LoginURL(),
					},
				},
				"recovery": map[string]interface{}{
					"enabled": true,
					"ui_url":  selfService.RecoveryURL(),
				},
				"registration": map[string]interface{}{
					"after": map[string]interface{}{
						"password": map[string]interface{}{
							"hooks": []map[string]interface{}{
								{
									"hook": "session",
								},
							},
						},
					},
					"lifespan": "10m",
					"ui_url":   selfService.RegistrationURL(),
				},
				"settings": map[string]interface{}{
					"privileged_session_max_age": "15m",
					"ui_url":                     selfService.SettingsURL(),
				},
				"verification": map[string]interface{}{
					"after": map[string]interface{}{
						"default_browser_return_url": selfService.LoginURL(),
					},
					"enabled": true,
					"ui_url":  selfService.VerificationURL(),
				},
			},
		},
	}
}

const kratosTestIdentity = `{
	"$id": "identity:test/regular",
    "$schema": "http://json-schema.org/draft-07/schema#",
    "title": "TestIdentity",
    "type": "object",
    "properties": {
      "traits": {
        "type": "object",
        "properties": {
          "email": {
            "type": "string",
            "format": "email",
            "title": "E-Mail",
            "minLength": 3,
            "ory.sh/kratos": {
              "credentials": {
                "password": {
                  "identifier": true
                }
              },
              "verification": {
                "via": "email"
              },
              "recovery": {
                "via": "email"
              }
            }
		  },
		  "firstName": {
			"type": "string"
		  },
		  "lastName": {
			"type": "string"
		  }
        },
        "required": [
		  "email",
		  "firstName",
		  "lastName"
        ],
        "additionalProperties": false
      }
    }
}`

// TestSelfServiceConfiguration defines a test Kratos selfservice handler.
type TestSelfServiceConfiguration interface {
	// URLs
	BasePublicURL() string
	DefaultBrowserReturnURL() string
	ErrorURL() string
	LoginURL() string
	RecoveryURL() string
	RegistrationURL() string
	SettingsURL() string
	VerificationURL() string
	WhitelistedReturnURLs() []string
}

// TestSelfService is the test default self service server.
type TestSelfService interface {
	TestSelfServiceConfiguration
	// Closes any resources allocated by this server.
	Close()
	ServeHTTP(w http.ResponseWriter, r *http.Request)

	//
	InitBrowserRegistration() error
	// Handlers:
	SetDefaultHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetErrorHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetLoginHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetRecoveryHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetRegistrationHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetSettingsHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	SetVerificationHandler(func(http.ResponseWriter, *http.Request)) TestSelfService
	// Other setters:
	SetKratosTestEnvCtx(TestEnvContext) TestSelfService
}

type defaultKratosTestSelfService struct {
	testContext *testing.T
	testServer  *httptest.Server
	httpClient  *http.Client

	kratosCtx TestEnvContext

	defaultHandlerFunc      func(http.ResponseWriter, *http.Request)
	errorHandlerFunc        func(http.ResponseWriter, *http.Request)
	loginHandlerFunc        func(http.ResponseWriter, *http.Request)
	recoveryHandlerFunc     func(http.ResponseWriter, *http.Request)
	registrationHandlerFunc func(http.ResponseWriter, *http.Request)
	settingsHandlerFunc     func(http.ResponseWriter, *http.Request)
	verificationHandlerFunc func(http.ResponseWriter, *http.Request)
}

// DefaultKratosTestSelfService returns an instance of the default self service provider.
func DefaultKratosTestSelfService(t *testing.T) TestSelfService {

	jar, _ := cookiejar.New(&cookiejar.Options{})
	httpClient := &http.Client{
		Jar: jar,
	}

	service := &defaultKratosTestSelfService{httpClient: httpClient, testContext: t}
	service.testServer = httptest.NewServer(service)
	service.testContext.Log("Kratos self service bound on", service.testServer.URL)
	return service
}

func (h *defaultKratosTestSelfService) Close() {
	h.testServer.Close()
}

func (h *defaultKratosTestSelfService) InitBrowserRegistration() error {
	return h.kratosCtx.ClientPublic().InitializeSelfServiceRegistrationViaBrowserFlow(
		kratosPublic.NewInitializeSelfServiceRegistrationViaBrowserFlowParams().
			WithHTTPClient(h.httpClient))
}

func (h *defaultKratosTestSelfService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		if h.defaultHandlerFunc != nil {
			h.defaultHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: default handler triggered")
			fmt.Fprint(w, "ok")
		}
	case "/app/error":
		if h.errorHandlerFunc != nil {
			h.errorHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: error handler triggered")
			fmt.Fprint(w, "ok")
		}
	case "/app/login":
		if h.loginHandlerFunc != nil {
			h.loginHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: login handler triggered")
			fmt.Fprint(w, "ok")
		}
	case "/app/recovery":
		if h.recoveryHandlerFunc != nil {
			h.recoveryHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: recovery handler triggered")
			fmt.Fprint(w, "ok")
		}
	case "/app/registration":
		if h.registrationHandlerFunc != nil {
			h.registrationHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: registration handler triggered")
			_, err := h.kratosCtx.ClientPublic().GetSelfServiceRegistrationFlow(kratosPublic.
				NewGetSelfServiceRegistrationFlowParams().
				WithHTTPClient(h.httpClient).
				WithID(r.URL.Query().Get("flow")))
			if err != nil {
				h.testContext.Error(err)
			}
		}
	case "/app/settings":
		if h.settingsHandlerFunc != nil {
			h.settingsHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: settings handler triggered")
			fmt.Fprint(w, "ok")
		}
	case "/app/verification":
		if h.verificationHandlerFunc != nil {
			h.loginHandlerFunc(w, r)
		} else {
			h.testContext.Log("DefaultKratosTestSelfService: verification handler triggered")
			fmt.Fprint(w, "ok")
		}
	}
}

func (h *defaultKratosTestSelfService) SetKratosTestEnvCtx(ctx TestEnvContext) TestSelfService {
	h.kratosCtx = ctx
	return h
}

func (h *defaultKratosTestSelfService) BasePublicURL() string {
	return ""
}
func (h *defaultKratosTestSelfService) DefaultBrowserReturnURL() string {
	return fmt.Sprintf("%s/", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) ErrorURL() string {
	return fmt.Sprintf("%s/app/error", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) LoginURL() string {
	return fmt.Sprintf("%s/app/login", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) RecoveryURL() string {
	return fmt.Sprintf("%s/app/recovery", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) RegistrationURL() string {
	return fmt.Sprintf("%s/app/registration", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) SettingsURL() string {
	return fmt.Sprintf("%s/app/settings", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) VerificationURL() string {
	return fmt.Sprintf("%s/app/verification", h.testServer.URL)
}
func (h *defaultKratosTestSelfService) WhitelistedReturnURLs() []string {
	return []string{h.testServer.URL}
}

func (h *defaultKratosTestSelfService) SetDefaultHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.defaultHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetErrorHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.errorHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetLoginHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.loginHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetRecoveryHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.recoveryHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetRegistrationHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.registrationHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetSettingsHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.settingsHandlerFunc = handler
	return h
}
func (h *defaultKratosTestSelfService) SetVerificationHandler(handler func(http.ResponseWriter, *http.Request)) TestSelfService {
	h.verificationHandlerFunc = handler
	return h
}

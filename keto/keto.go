package keto

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"
	"gopkg.in/yaml.v2"

	// postgres library
	_ "github.com/lib/pq"

	ketoClient "github.com/ory/keto-client-go/client"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/postgres"
)

const (
	// DefaultKetoImageName specifies the Keto docker image name to use in tests.
	DefaultKetoImageName = "oryd/keto"
	// DefaultKetoEnvVarImageName is the environment variable name for default Keto docker image name.
	DefaultKetoEnvVarImageName = "TEST_ORY_KETO_IMAGE_NAME"
	// DefaultKetoImageVersion specifies the Keto docker image version to use in tests.
	DefaultKetoImageVersion = "v0.6.0-alpha.1"
	// DefaultKetoEnvVarImageVersion is the environment variable name for default Keto docker image version.
	DefaultKetoEnvVarImageVersion = "TEST_ORY_KETO_IMAGE_VERSION"
)

// TestEnvContext represents a test keto environment context.
type TestEnvContext interface {
	Cleanup()
	WriteClient() *ketoClient.OryKeto
	ReadClient() *ketoClient.OryKeto
	Pool() *dockertest.Pool
	WritePort() string
	ReadPort() string
}

// SetupTestKeto sets up keto environment for tests.
func SetupTestKeto(t *testing.T, postgresCtx postgres.TestEnvContext, namespace ...string) TestEnvContext {

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
	ketoConfig, tempFileErr := ioutil.TempFile("", "keto-config")
	if tempFileErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Keto configuration file to be created but received an error: '%v'", tempFileErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: removing keto config")
		os.Remove(ketoConfig.Name())
	}, closables)

	// create a temp directory to hold namespaces:
	ketoNamespacesDir, tempDirErr := ioutil.TempDir("", "keto-namespaces")
	if tempDirErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Keto namespaces directory to be created but received an error: '%v'", tempDirErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: removing keto namespaces directory")
		os.RemoveAll(ketoNamespacesDir)
	}, closables)

	// we need two random ports:
	writeListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating write random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing write random port listener, if not closed yet")
		writeListener.Cleanup()
	}, closables)
	readListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating read random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing read random port listener, if not closed yet")
		readListener.Cleanup()
	}, closables)

	if err := writeListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from write random port listener: '%v'", err)
	}
	if err := readListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from read random port listener: '%v'", err)
	}

	// Start keto:

	fetchedKetoWritePort, _ := writeListener.DiscoveredPort()
	fetchedKetoReadPort, _ := readListener.DiscoveredPort()

	// write test configuration to the temp file:

	ketoSerializedConfigBytes, yamlErr := yaml.Marshal(ketoTestConfigFunc(t, fetchedKetoWritePort, fetchedKetoReadPort, namespace))
	if yamlErr != nil {
		closeClosables(closables)
		t.Fatalf("failed serializing Hydra config to YAML: '%v'", err)
	}

	if writeErr := ioutil.WriteFile(ketoConfig.Name(), ketoSerializedConfigBytes, 0777); writeErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp Hydra configuration file to be written but received an error: '%v'", writeErr)
	}

	t.Log("Keto config written...", ketoConfig.Name())

	// create new pool using the default Docker endpoint:
	pool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		closeClosables(closables)
		t.Fatalf("expected docker pool to come up but received: '%v'", poolErr)
	}

	// Now, we need to run keto migrate, postgres is starting but keto retries, we need to wait for it...
	migrateOptions := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultKetoEnvVarImageName, DefaultKetoImageName),
		Tag:        common.GetEnvOrDefault(DefaultKetoEnvVarImageVersion, DefaultKetoImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/keto/keto.yml", ketoConfig.Name()),
			fmt.Sprintf("%s:/keto_namespaces", ketoNamespacesDir),
		},
		Cmd: []string{"migrate", "up", "--all-namespaces", "-c", "/etc/config/keto/keto.yml", "--yes"},
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
	ketoMigrate, ketoErr := pool.RunWithOptions(migrateOptions, func(config *dc.HostConfig) {
		// Keto migrate retries automatically but for compatibility, we make sure it does...
		config.RestartPolicy.Name = "on-failure"
	})
	if ketoErr != nil {
		closeClosables(closables)
		t.Fatalf("expected keto migrate to start but received: '%v'", ketoErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: closing keto migrate")
		ketoMigrate.Close()
		pool.Purge(ketoMigrate)
	}, closables)

	if err := common.WaitForContainerExit0(t, pool, ketoMigrate.Container.ID); err != nil {
		closeClosables(closables)
		t.Fatalf("error while waiting for container to finish with exist status 0: '%v'", err)
	}

	// create test keto run options:
	options := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultKetoEnvVarImageName, DefaultKetoImageName),
		Tag:        common.GetEnvOrDefault(DefaultKetoEnvVarImageVersion, DefaultKetoImageVersion),
		Mounts: []string{
			fmt.Sprintf("%s:/etc/config/keto/keto.yml", ketoConfig.Name()),
			fmt.Sprintf("%s:/keto_namespaces", ketoNamespacesDir),
		},
		Cmd: []string{"serve", "-c", "/etc/config/keto/keto.yml", "all"},
		ExposedPorts: []string{
			fmt.Sprintf("%s/tcp", fetchedKetoWritePort),
			fmt.Sprintf("%s/tcp", fetchedKetoReadPort)},
		PortBindings: map[dc.Port][]dc.PortBinding{
			dc.Port(fmt.Sprintf("%s/tcp", fetchedKetoWritePort)): {{HostIP: "0.0.0.0", HostPort: fetchedKetoWritePort}},
			dc.Port(fmt.Sprintf("%s/tcp", fetchedKetoReadPort)):  {{HostIP: "0.0.0.0", HostPort: fetchedKetoReadPort}},
		},
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

	// make sure the random keto ports are free before we request the container to start:
	writeListener.Cleanup()
	readListener.Cleanup()

	// start the container:
	keto, ketoErr := pool.RunWithOptions(options, func(config *dc.HostConfig) {})
	if ketoErr != nil {
		closeClosables(closables)
		t.Fatalf("expected keto to start but received: '%v'", ketoErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing keto")
		keto.Close()
		pool.Purge(keto)
	}, closables)

	benchStart := time.Now()

	chanKetoWriteAPISuccess := make(chan struct{}, 1)
	chanKetoWriteAPIError := make(chan error, 1)

	chanKetoReadAPISuccess := make(chan struct{}, 1)
	chanKetoReadAPIError := make(chan error, 1)

	ketoWritePort := keto.GetPort(fmt.Sprintf("%s/tcp", fetchedKetoWritePort))
	ketoReadPort := keto.GetPort(fmt.Sprintf("%s/tcp", fetchedKetoReadPort))

	t.Logf("Keto started with container ID '%s'", keto.Container.ID)
	t.Log(" ==> waiting for the write REST API to start replying...")

	apiTestFunc := func(callPort string, chanSuccess chan struct{}, chanError chan error) func() {
		return func() {
			poolRetryErr := pool.Retry(func() error {
				request, requestErr := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%s/health/alive", callPort), nil)
				if requestErr != nil {
					t.Logf("expected /health/alive request to be constructed but received: '%v'", requestErr)
					return requestErr
				}
				httpClient := &http.Client{}
				response, responseErr := httpClient.Do(request)
				if responseErr != nil {
					t.Logf("expected /health/alive to reply but received: '%v'", responseErr)
					return responseErr
				}
				responseBytes, responseReadErr := ioutil.ReadAll(response.Body)
				if responseErr != nil {
					t.Logf("failed reading response body: '%v'", responseReadErr)
					return responseErr
				}
				defer response.Body.Close()
				t.Log("keto /health/alive replied with status: ", response.StatusCode, string(responseBytes))
				if response.StatusCode == http.StatusOK {
					t.Logf("keto /health/alive replied with status OK, body '%s'", string(responseBytes))
					return nil
				}
				t.Logf("keto /health/alive replied with status other than OK: '%d', body: '%s'", response.StatusCode, string(responseBytes))
				return fmt.Errorf("keto rest status code other than OK: %d", response.StatusCode)
			})
			if poolRetryErr == nil {
				close(chanSuccess)
				return
			}
			chanError <- poolRetryErr
		}
	}

	go apiTestFunc(ketoWritePort, chanKetoWriteAPISuccess, chanKetoWriteAPIError)()

	select {
	case <-chanKetoWriteAPISuccess:
		t.Logf("keto REST API replied after: %s", time.Now().Sub(benchStart).String())
	case receivedError := <-chanKetoWriteAPIError:
		closeClosables(closables)
		t.Fatalf("keto REST API wait finished with error: '%v'", receivedError)
	case <-time.After(time.Second * 30):
		closeClosables(closables)
		t.Fatalf("keto REST API did not start communicating within timeout")
	}

	t.Logf(" ==> waiting for the read REST API to start replying...")

	go apiTestFunc(ketoReadPort, chanKetoReadAPISuccess, chanKetoReadAPIError)()

	select {
	case <-chanKetoReadAPISuccess:
		t.Logf("keto REST API replied after: %s", time.Now().Sub(benchStart).String())
	case receivedError := <-chanKetoReadAPIError:
		closeClosables(closables)
		t.Fatalf("keto REST API wait finished with error: '%v'", receivedError)
	case <-time.After(time.Second * 30):
		closeClosables(closables)
		t.Fatalf("keto REST API did not start communicating within timeout")
	}

	return &testKetoContext{
		CleanupFuncValue: func() {
			for _, closable := range closables {
				closable()
			}
		},
		WriteClientValue: ketoClient.NewHTTPClientWithConfig(nil,
			ketoClient.
				DefaultTransportConfig().
				WithSchemes([]string{"http"}).
				WithHost(fmt.Sprintf("localhost:%s", ketoWritePort))),
		ReadClientValue: ketoClient.NewHTTPClientWithConfig(nil,
			ketoClient.
				DefaultTransportConfig().
				WithSchemes([]string{"http"}).
				WithHost(fmt.Sprintf("localhost:%s", ketoReadPort))),
		PoolValue:      pool,
		WritePortValue: ketoWritePort,
		ReadPortValue:  ketoReadPort,
	}

}

type testKetoContext struct {
	CleanupFuncValue func()
	WriteClientValue *ketoClient.OryKeto
	ReadClientValue  *ketoClient.OryKeto
	PoolValue        *dockertest.Pool
	WritePortValue   string
	ReadPortValue    string
}

func (ctx *testKetoContext) Cleanup() {
	ctx.CleanupFuncValue()
}

func (ctx *testKetoContext) WriteClient() *ketoClient.OryKeto {
	return ctx.WriteClientValue
}

func (ctx *testKetoContext) ReadClient() *ketoClient.OryKeto {
	return ctx.ReadClientValue
}

func (ctx *testKetoContext) Pool() *dockertest.Pool {
	return ctx.PoolValue
}

func (ctx *testKetoContext) WritePort() string {
	return ctx.WritePortValue
}

func (ctx *testKetoContext) ReadPort() string {
	return ctx.ReadPortValue
}

func ketoTestConfigFunc(t *testing.T, writePort, readPort string, namespaces []string) map[string]interface{} {
	return map[string]interface{}{
		"namespaces": func() []*ketoNamespace {
			items := []*ketoNamespace{}
			for idx, ns := range namespaces {
				items = append(items, &ketoNamespace{
					ID:   idx,
					Name: ns,
				})
			}
			return items
		}(),
		"serve": map[string]interface{}{
			"write": &ketoServe{
				Host: "0.0.0.0",
				Port: func() int {
					i, _ := strconv.Atoi(writePort)
					return i
				}(),
			},
			"read": &ketoServe{
				Host: "0.0.0.0",
				Port: func() int {
					i, _ := strconv.Atoi(readPort)
					return i
				}(),
			},
		},
	}
}

type ketoNamespace struct {
	ID   int    `yaml:"id"`
	Name string `yaml:"name"`
}
type ketoServe struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

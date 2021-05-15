package postgres

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"

	// Posgres library:
	_ "github.com/lib/pq"

	"github.com/radekg/app-kit-orytest/common"
)

const (
	// DefaultPostgresImageName specifies the Postgres docker image name to use in tests.
	DefaultPostgresImageName = "postgres"
	// DefaultPostgresEnvVarImageName is the environment variable name for default Postgres docker image name.
	DefaultPostgresEnvVarImageName = "TEST_POSTGRES_IMAGE_NAME"
	// DefaultPostgresImageVersion specifies the Postgres docker image version to use in tests.
	DefaultPostgresImageVersion = "13.2"
	// DefaultPostgresEnvVarImageVersion is the environment variable name for default Postgres docker image version.
	DefaultPostgresEnvVarImageVersion = "TEST_POSTGRES_IMAGE_VERSION"
)

// TestEnvContext represents a test Postgres environment context.
type TestEnvContext interface {
	Cleanup()
	ContainerIP() string
	ContainerPrivatePort() string
	ContainerPublicPort() string
	PostgresDatabase() string
	PostgresPass() string
	PostgresUser() string
}

// SetupTestPostgres sets up Postgres environment for tests.
// This environment can be passed to other Ory components in tests.
func SetupTestPostgres(t *testing.T) TestEnvContext {

	// after every successful step, store a cleanup function here:
	closables := []func(){}

	postgresUser := "test"
	postgresPass := "test"
	postgresDb := "test"
	postgresPort := "5432"

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

	// Using Postgres for Kratos migrations

	postgresListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating postgres random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing postgres random port listener, if not closed yet")
		postgresListener.Cleanup()
	}, closables)

	if err := postgresListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from postgres random port listener: '%v'", err)
	}

	fetchedPostgresRandomPort, _ := postgresListener.DiscoveredPort()

	postgresOptions := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultPostgresEnvVarImageName, DefaultPostgresImageName),
		Tag:        common.GetEnvOrDefault(DefaultPostgresEnvVarImageVersion, DefaultPostgresImageVersion),
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%s", postgresUser),
			fmt.Sprintf("POSTGRES_PASSWORD=%s", postgresPass),
			fmt.Sprintf("POSTGRES_DB=%s", postgresDb),
		},
		ExposedPorts: []string{
			fmt.Sprintf("%s/tcp", postgresPort)},
		PortBindings: map[dc.Port][]dc.PortBinding{
			dc.Port(fmt.Sprintf("%s/tcp", postgresPort)): {{HostIP: "0.0.0.0", HostPort: fetchedPostgresRandomPort}}},
	}

	// make sure the random postgres port is free before we request the container to start:
	postgresListener.Cleanup()

	// create new pool using the default Docker endpoint:
	pool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		closeClosables(closables)
		t.Fatalf("expected docker pool to come up but received: '%v'", poolErr)
	}

	// start postgres:
	postgres, postgresErr := pool.RunWithOptions(postgresOptions, func(config *dc.HostConfig) {})
	if postgresErr != nil {
		closeClosables(closables)
		t.Fatalf("expected postgres to start but received: '%v'", postgresErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: closing postgres")
		postgres.Close()
		pool.Purge(postgres)
	}, closables)

	// wait until we can connect to postgres:
	for {
		db, sqlOpenErr := sql.Open("postgres", fmt.Sprintf("host=localhost port=%s user=%s password=%s dbname=%s sslmode=disable",
			fetchedPostgresRandomPort, postgresUser, postgresPass, postgresDb))
		if sqlOpenErr == nil {
			t.Log("connected to postgres")
			db.Close()
			break
		}
		t.Log("Still waiting for postgres...")
		<-time.After(time.Second)
	}

	postgresIPAddress := ""
	containers, _ := pool.Client.ListContainers(dc.ListContainersOptions{All: true})
	for _, container := range containers {
		if container.ID == postgres.Container.ID {
			for _, network := range container.Networks.Networks {
				postgresIPAddress = network.IPAddress
			}
		}
	}

	return &testPostgresContext{
		CleanupFuncValue: func() {
			for _, closable := range closables {
				closable()
			}
		},
		ContainerIPValue:          postgresIPAddress,
		ContainerPrivatePortValue: postgresPort,
		ContainerPublicPortValue:  fetchedPostgresRandomPort,
		PostgresDatabaseValue:     postgresDb,
		PostgresPassValue:         postgresPass,
		PostgresUserValue:         postgresUser,
	}

}

type testPostgresContext struct {
	CleanupFuncValue          func()
	ContainerIPValue          string
	ContainerPrivatePortValue string
	ContainerPublicPortValue  string
	PostgresDatabaseValue     string
	PostgresPassValue         string
	PostgresUserValue         string
}

func (ctx *testPostgresContext) Cleanup() {
	ctx.CleanupFuncValue()
}

func (ctx *testPostgresContext) ContainerIP() string {
	return ctx.ContainerIPValue
}
func (ctx *testPostgresContext) ContainerPrivatePort() string {
	return ctx.ContainerPrivatePortValue
}
func (ctx *testPostgresContext) ContainerPublicPort() string {
	return ctx.ContainerPublicPortValue
}
func (ctx *testPostgresContext) PostgresDatabase() string {
	return ctx.PostgresDatabaseValue
}
func (ctx *testPostgresContext) PostgresPass() string {
	return ctx.PostgresPassValue
}
func (ctx *testPostgresContext) PostgresUser() string {
	return ctx.PostgresUserValue
}

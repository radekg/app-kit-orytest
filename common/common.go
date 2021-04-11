package common

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"
)

// GetEnvOrDefault returns the value of an environment variable
// or fallback value, if environment variable is undefined or empty.
func GetEnvOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return fallback
	}
	return value
}

// StringP returns a pointer of a given input.
func StringP(input string) *string {
	return &input
}

// BoolP returns a pointer of a given input.
func BoolP(input bool) *bool {
	return &input
}

// Int64P returns a pointer of a given input.
func Int64P(input int64) *int64 {
	return &input
}

// -- Random port supplier.

// RandomPortSupplier wraps the functionality for random port handling in tests.
type RandomPortSupplier interface {
	Cleanup()
	Discover() error
	DiscoveredHost() (string, bool)
	DiscoveredPort() (string, bool)
}

type listenerPortSupplier struct {
	closed         bool
	discovered     bool
	discoveredHost string
	discoveredPort string
	listener       net.Listener
	lock           *sync.Mutex
}

// NewRandomPortSupplier creates an initialized instance of a random port supplier.
func NewRandomPortSupplier() (RandomPortSupplier, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &listenerPortSupplier{
		lock:     &sync.Mutex{},
		listener: listener,
	}, nil
}

func (l *listenerPortSupplier) Cleanup() {
	l.lock.Lock()
	defer l.lock.Unlock()
	if !l.closed {
		l.listener.Close()
		l.closed = true
	}
}

func (l *listenerPortSupplier) Discover() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.closed {
		return errors.New("was-closed")
	}
	host, port, err := net.SplitHostPort(l.listener.Addr().String())
	if err != nil {
		return err
	}
	l.discoveredHost = host
	l.discoveredPort = port
	l.discovered = true
	return nil
}

func (l *listenerPortSupplier) DiscoveredHost() (string, bool) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.discoveredHost, l.discovered
}

func (l *listenerPortSupplier) DiscoveredPort() (string, bool) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.discoveredPort, l.discovered
}

// WaitForContainerExit0 waits for the container to exist with code 0.
func WaitForContainerExit0(t *testing.T, pool *dockertest.Pool, containerID string) error {
	finalState := "not started"
	finalStatus := ""

	benchMigrateStart := time.Now()
	chanSuccess := make(chan struct{}, 1)
	chanError := make(chan error, 1)

	go func() {
		poolRetryErr := pool.Retry(func() error {
			containers, _ := pool.Client.ListContainers(dc.ListContainersOptions{All: true})
			for _, container := range containers {
				if container.ID == containerID {
					time.Sleep(time.Millisecond * 50)
					if container.State == "running" {
						return errors.New("still running")
					}
					if container.State == "restarting" {
						t.Logf("container %s is restarting with status '%s'...", containerID, container.Status)
						time.Sleep(time.Second)
						continue
					}
					finalState = container.State
					finalStatus = container.Status
					return nil
				}
			}
			return errors.New("no container")
		})
		if poolRetryErr == nil {
			close(chanSuccess)
			return
		}
		chanError <- poolRetryErr
	}()

	select {
	case <-chanSuccess:
		t.Logf("container %s finished successfully after: %s", containerID, time.Now().Sub(benchMigrateStart).String())
	case receivedError := <-chanError:
		return receivedError
	case <-time.After(time.Second * 10):
		return fmt.Errorf("container %s complete within timeout", containerID)
	}

	if finalState != "exited" {
		return fmt.Errorf("expected container %s to be in state exited but received: '%s'", containerID, finalState)
	}
	// it was exited, ...
	if !strings.HasPrefix(strings.ToLower(finalStatus), "exited (0)") {
		return fmt.Errorf("expected container %s to exit with status 0, received full exit message: '%s'", containerID, finalStatus)
	}

	return nil
}

// CompareStringSlices cpmpares if two string slices have the same length and same values.
func CompareStringSlices(t *testing.T, this, that []string) {
	if len(this) != len(that) {
		t.Fatalf("expected did not match received: '%v' vs '%v'", this, that)
	}
	for idx, exp := range this {
		if exp != that[idx] {
			t.Fatalf("expected did not match received at index %d: '%v' vs '%v'", idx, exp, that[idx])
		}
	}
}

// StringSlicesEqual compares two slices without failing the test.
func StringSlicesEqual(t *testing.T, this, that []string) bool {
	if len(this) != len(that) {
		return false
	}
	for idx, exp := range this {
		if exp != that[idx] {
			return false
		}
	}
	return true
}

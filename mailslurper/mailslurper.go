package mailslurper

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest"
	dc "github.com/ory/dockertest/docker"

	"github.com/radekg/app-kit-orytest/common"
)

const (
	// DefaultMailslurperImageName specifies the Mailslurper docker image name to use in tests.
	DefaultMailslurperImageName = "oryd/mailslurper"
	// DefaultMailslurperEnvVarImageName is the environment variable name for default Mailslurper docker image name.
	DefaultMailslurperEnvVarImageName = "TEST_ORY_MAILSLURPER_IMAGE_NAME"
	// DefaultMailslurperImageVersion specifies the Mailslurper docker image version to use in tests.
	DefaultMailslurperImageVersion = "smtps-latest"
	// DefaultMailslurperEnvVarImageVersion is the environment variable name for default Mailslurper docker image version.
	DefaultMailslurperEnvVarImageVersion = "TEST_ORY_MAILSLURPER_IMAGE_VERSION"
	// DefaultSubjectRecoverAccount is used when retrieving account recovery email
	DefaultSubjectRecoverAccount = "Recover access to your account"
	// DefaultSubjectVerifyAccount is used when retrieving account verification email
	DefaultSubjectVerifyAccount = "Please verify your email address"
)

// TestEnvContext represents a test Mailslurper environment context.
type TestEnvContext interface {
	Cleanup()
	ContainerIP() string
	ContainerWWWPort() string
	ContainerServicePort() string
	ContainerSMTPPort() string

	ExtractRecoveryLink(*MailItem) (string, error)
	ExtractVerificationLink(*MailItem) (string, error)
	ListEmail() (*Mail, error)
	WaitForRecoveryEmail(context.Context, *testing.T, string) <-chan interface{}
	WaitForVerificationEmail(context.Context, *testing.T, string) <-chan interface{}
	WaitForEmailWithSubject(context.Context, *testing.T, string, string) <-chan interface{}
}

// SetupTestMailslurper sets up Mailslurper environment for tests.
// This environment can be passed to other Ory components in tests.
func SetupTestMailslurper(t *testing.T) TestEnvContext {

	cleanupContainers := true

	// after every successful step, store a cleanup function here:
	closables := []func(){}

	// close all reasources in reverse order:
	prependClosable := func(closable func(), closables []func()) []func() {
		return append([]func(){closable}, closables...)
	}

	// used in case of a failure during setup:
	closeClosables := func(closables []func()) {
		for _, closable := range closables {
			defer closable()
		}
	}

	// mailslurper ports:
	mailslurperWWWListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating mailslurper WWW random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing mailslurper WWW random port listener, if not closed yet")
		mailslurperWWWListener.Cleanup()
	}, closables)

	mailslurperServiceListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating mailslurper service random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing mailslurper service random port listener, if not closed yet")
		mailslurperServiceListener.Cleanup()
	}, closables)

	mailslurperSMTPListener, err := common.NewRandomPortSupplier()
	if err != nil {
		closeClosables(closables)
		t.Fatalf("failed creating mailslurper SMTP random port listener: '%v'", err)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: closing mailslurper SMTP random port listener, if not closed yet")
		mailslurperSMTPListener.Cleanup()
	}, closables)

	if err := mailslurperWWWListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from mailslurper WWW random port listener: '%v'", err)
	}
	if err := mailslurperServiceListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from mailslurper service random port listener: '%v'", err)
	}
	if err := mailslurperSMTPListener.Discover(); err != nil {
		closeClosables(closables)
		t.Fatalf("failed extracting host and port from mailslurper SMTP random port listener: '%v'", err)
	}

	fetchedMailslurperWWWRandomPort, _ := mailslurperWWWListener.DiscoveredPort()
	fetchedMailslurperServiceRandomPort, _ := mailslurperServiceListener.DiscoveredPort()
	fetchedMailslurperSMTPRandomPort, _ := mailslurperSMTPListener.DiscoveredPort()

	// create new pool using the default Docker endpoint:
	pool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		closeClosables(closables)
		t.Fatalf("expected docker pool to come up but received: '%v'", poolErr)
	}

	// create mailslurper config:
	mailslurperConfig, tempFileErr := ioutil.TempFile("", "mailslurper-config")
	if tempFileErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp mailslurper config file to be created but received an error: '%v'", tempFileErr)
	}

	mailslurperJSONConfig := mailslurperConfigFunc(fetchedMailslurperWWWRandomPort, fetchedMailslurperServiceRandomPort, fetchedMailslurperSMTPRandomPort)
	if writeErr := ioutil.WriteFile(mailslurperConfig.Name(), []byte(mailslurperJSONConfig), 0777); writeErr != nil {
		closeClosables(closables)
		t.Fatalf("expected temp mailslurper configuration file to be written but received an error: '%v'", writeErr)
	}
	closables = prependClosable(func() {
		t.Log("cleanup: removing mailslurper configuration")
		os.Remove(mailslurperConfig.Name())
	}, closables)

	t.Log("Mailslurper configuration written...", mailslurperConfig.Name())

	// start mailslurper:
	mailslurperOptions := &dockertest.RunOptions{
		Repository: common.GetEnvOrDefault(DefaultMailslurperEnvVarImageName, DefaultMailslurperImageName),
		Tag:        common.GetEnvOrDefault(DefaultMailslurperEnvVarImageVersion, DefaultMailslurperImageVersion),
		ExposedPorts: []string{
			fmt.Sprintf("%s/tcp", fetchedMailslurperWWWRandomPort),
			fmt.Sprintf("%s/tcp", fetchedMailslurperServiceRandomPort),
			fmt.Sprintf("%s/tcp", fetchedMailslurperSMTPRandomPort)},
		PortBindings: map[dc.Port][]dc.PortBinding{
			dc.Port(fmt.Sprintf("%s/tcp", fetchedMailslurperWWWRandomPort)):     {{HostIP: "0.0.0.0", HostPort: fetchedMailslurperWWWRandomPort}},
			dc.Port(fmt.Sprintf("%s/tcp", fetchedMailslurperServiceRandomPort)): {{HostIP: "0.0.0.0", HostPort: fetchedMailslurperServiceRandomPort}},
			dc.Port(fmt.Sprintf("%s/tcp", fetchedMailslurperSMTPRandomPort)):    {{HostIP: "0.0.0.0", HostPort: fetchedMailslurperSMTPRandomPort}},
		},
		Mounts: []string{
			fmt.Sprintf("%s:/go/src/github.com/mailslurper/mailslurper/cmd/mailslurper/config.json", mailslurperConfig.Name()),
		},
	}

	// make sure the random mailslurper ports are free before we request the container to start:
	mailslurperWWWListener.Cleanup()
	mailslurperServiceListener.Cleanup()
	mailslurperSMTPListener.Cleanup()

	mailslurper, kratosErr := pool.RunWithOptions(mailslurperOptions, func(config *dc.HostConfig) {})
	if kratosErr != nil {
		closeClosables(closables)
		t.Fatalf("expected mailslurper to start but received: '%v'", kratosErr)
	}

	closables = prependClosable(func() {
		t.Log("cleanup: closing mailslurper")
		if cleanupContainers {
			mailslurper.Close()
			pool.Purge(mailslurper)
		}
	}, closables)

	mailslurperIPAddress := ""
	containers, _ := pool.Client.ListContainers(dc.ListContainersOptions{All: true})
	for _, container := range containers {
		if container.ID == mailslurper.Container.ID {
			for _, network := range container.Networks.Networks {
				mailslurperIPAddress = network.IPAddress
			}
		}
	}

	return &testMailslurperContext{
		CleanupValue: func() {
			for _, closable := range closables {
				closable()
			}
		},
		ContainerIPValue:          mailslurperIPAddress,
		ContainerWWWPortValue:     fetchedMailslurperWWWRandomPort,
		ContainerServicePortValue: fetchedMailslurperServiceRandomPort,
		ContainerSMTPPortValue:    fetchedMailslurperSMTPRandomPort,
	}

}

type testMailslurperContext struct {
	CleanupValue              func()
	ContainerIPValue          string
	ContainerWWWPortValue     string
	ContainerServicePortValue string
	ContainerSMTPPortValue    string
}

func (ctx *testMailslurperContext) Cleanup() {
	ctx.CleanupValue()
}
func (ctx *testMailslurperContext) ContainerIP() string {
	return ctx.ContainerIPValue
}
func (ctx *testMailslurperContext) ContainerWWWPort() string {
	return ctx.ContainerWWWPortValue
}
func (ctx *testMailslurperContext) ContainerServicePort() string {
	return ctx.ContainerServicePortValue
}
func (ctx *testMailslurperContext) ContainerSMTPPort() string {
	return ctx.ContainerSMTPPortValue
}

func (ctx *testMailslurperContext) ExtractRecoveryLink(mailItem *MailItem) (string, error) {
	// this is the email message we've been waiting for:
	re := regexp.MustCompile("\\<a.*\\>(.*)\\<\\/a\\>")
	match := re.FindStringSubmatch(mailItem.Body)
	if len(match) == 0 {
		return "", fmt.Errorf("recovery email received but recovery link could not be extracted, body: '%s'", mailItem.Body)
	}
	return match[1], nil
}

func (ctx *testMailslurperContext) ExtractVerificationLink(mailItem *MailItem) (string, error) {
	// this is the email message we've been waiting for:
	re := regexp.MustCompile("\\<a.*\\>(.*)\\<\\/a\\>")
	match := re.FindStringSubmatch(mailItem.Body)
	if len(match) == 0 {
		return "", fmt.Errorf("verification email received but verification link could not be extracted, body: '%s'", mailItem.Body)
	}
	return match[1], nil
}

func (ctx *testMailslurperContext) ListEmail() (*Mail, error) {
	httpClient := &http.Client{}
	response, err := httpClient.Get(fmt.Sprintf("http://localhost:%s/mail?pageNumber=1", ctx.ContainerServicePortValue))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	mail := &Mail{}
	return mail, json.NewDecoder(response.Body).Decode(mail)
}

func (ctx *testMailslurperContext) WaitForRecoveryEmail(c context.Context, t *testing.T, recipient string) <-chan interface{} {
	return ctx.WaitForEmailWithSubject(c, t, recipient, DefaultSubjectRecoverAccount)
}

func (ctx *testMailslurperContext) WaitForVerificationEmail(c context.Context, t *testing.T, recipient string) <-chan interface{} {
	return ctx.WaitForEmailWithSubject(c, t, recipient, DefaultSubjectVerifyAccount)
}

func (ctx *testMailslurperContext) WaitForEmailWithSubject(c context.Context, t *testing.T, recipient, subject string) <-chan interface{} {
	callCtx, cancelFunc := context.WithDeadline(c, time.Now().Add(time.Second*10))

	respChan := make(chan interface{}, 1)

	go func() {
		defer close(respChan)
		for {
			if err := callCtx.Err(); err != nil {
				if err.Error() == context.Canceled.Error() {
					return // message has been delivered
				}
				respChan <- err
				return
			}
			mail, err := ctx.ListEmail()
			if err != nil {
				cancelFunc()
				respChan <- fmt.Errorf("expected mail but received an error: '%v'", err)
				return
			}
			if mail.TotalRecords == 0 {
				time.Sleep(time.Second)
			} else {
				for _, mailItem := range mail.MailItems {
					if strings.TrimSpace(mailItem.Subject) == strings.TrimSpace(subject) && common.StringSlicesEqual(t, mailItem.ToAddresses, []string{recipient}) {
						cancelFunc()
						respChan <- mailItem
						return
					}
				}
			}
		}

	}()

	return respChan
}

func mailslurperConfigFunc(wwwPort, servicePort, smtpPort string) string {
	return fmt.Sprintf(`{
		"wwwAddress": "0.0.0.0",
		"wwwPort": %s,
		"serviceAddress": "0.0.0.0",
		"servicePort": %s,
		"smtpAddress": "0.0.0.0",
		"smtpPort": %s,
		"dbEngine": "SQLite",
		"dbHost": "",
		"dbPort": 0,
		"dbDatabase": "mailslurper.db",
		"dbUserName": "",
		"dbPassword": "",
		"maxWorkers": 1000,
		"autoStartBrowser": false,
		"keyFile": "mailslurper-key.pem",
		"certFile": "mailslurper-cert.pem",
		"adminKeyFile": "",
		"adminCertFile": ""
	}`, wwwPort, servicePort, smtpPort)
}

// Mail represents mailslurper messages.
type Mail struct {
	MailItems    []*MailItem `json:"mailItems"`
	TotalPages   int         `json:"totalPages"`
	TotalRecords int         `json:"totalRecords"`
}

// MailItem represents a single mailslurper message.
type MailItem struct {
	ID                string        `json:"id"`
	DateSent          string        `json:"dateSent"`
	FromAddress       string        `json:"fromAddress"`
	ToAddresses       []string      `json:"toAddresses"`
	Subject           string        `json:"subject"`
	XMailer           string        `json:"xmailer"`
	MIMEVersion       string        `json:"mimeVersion"`
	Body              string        `json:"body"`
	ContentType       string        `json:"contentType"`
	Boundary          string        `json:"boundary"`
	Attachments       []interface{} `json:"attachments"`
	TransferEncoding  string        `json:"transferEncoding"`
	Message           interface{}   `json:"Message,omitempty"`
	InlineAttachments interface{}   `json:"InlineAttachments,omitempty"`
	TextBody          string        `json:"TextBody"`
	HTMLBody          string        `json:"HTMLBody"`
}

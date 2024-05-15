// Copyright 2022 Camunda Services GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/camunda/zeebe/clients/go/v8/pkg/entities"
	"github.com/camunda/zeebe/clients/go/v8/pkg/pb"
	"github.com/camunda/zeebe/clients/go/v8/pkg/worker"
	"github.com/camunda/zeebe/clients/go/v8/pkg/zbc"
	"golang.org/x/crypto/ssh"
)

//go:embed resources
var res embed.FS

const (
	kJobType    = "ssh"
	kWorkerName = "exec-worker"
	kHostname   = "hostname"
	kUsername   = "username"
	kPassword   = "password"
	kCommand    = "command"
)

func main() {
	shutdownBarrier := make(chan bool, 1)
	SetupShutdownBarrier(shutdownBarrier)

	client := MustCreateClient()
	defer MustCloseClient(client)
	MustDeployProcessDefinition(client)
	w := MustStartWorker(client)
	defer w.Close()
	MustStartProcessInstance(client)

	log.Printf("Log into tasklist to start doing so")
	log.Printf("use ctrl+c to exit or interrupt the application")

	<-shutdownBarrier
}

func SetupShutdownBarrier(done chan bool) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		done <- true
	}()
}

func MustReadFile(resourceFile string) []byte {
	contents, err := res.ReadFile("resources/" + resourceFile)
	if err != nil {
		panic(err)
	}

	return contents
}

func MustCreateClient() zbc.Client {
	credentials, err := zbc.NewOAuthCredentialsProvider(&zbc.OAuthProviderConfig{
		ClientID:               os.Getenv("ZEEBE_CLIENT_ID"),
		ClientSecret:           os.Getenv("ZEEBE_CLIENT_SECRET"),
		AuthorizationServerURL: os.Getenv("ZEEBE_AUTHORIZATION_SERVER_URL"),
		Audience:               os.Getenv("ZEEBE_TOKEN_AUDIENCE"),
	})
	if err != nil {
		panic(err)
	}

	config := zbc.ClientConfig{
		GatewayAddress:      os.Getenv("ZEEBE_ADDRESS"),
		CredentialsProvider: credentials,
	}

	client, err := zbc.NewClient(&config)
	if err != nil {
		panic(err)
	}

	return client
}

func MustCloseClient(client zbc.Client) {
	log.Println("closing client")
	_ = client.Close()
}

func MustDeployProcessDefinition(client zbc.Client) *pb.ProcessMetadata {
	definition := MustReadFile("ssh.bpmn")
	command := client.NewDeployResourceCommand().AddResource(definition, "ssh.bpmn")
	form := MustReadFile("ssh.form")
	command = command.AddResource(form, "ssh.form")

	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()

	resource, err := command.Send(ctx)
	if err != nil {
		panic(err)
	}

	if len(resource.GetDeployments()) == 0 {
		panic(errors.New("failed to deploy ssh model; nothing was deployed"))
	}

	deployment := resource.GetDeployments()[0]
	process := deployment.GetProcess()
	if process == nil {
		panic(errors.New("failed to deploy ssh process; the deployment was successful, but no process was returned"))
	}

	log.Printf("deployed BPMN process [%s] with key [%d]", process.GetBpmnProcessId(), process.GetProcessDefinitionKey())
	return process
}

func MustStartProcessInstance(client zbc.Client) *pb.CreateProcessInstanceResponse {
	command, err := client.NewCreateInstanceCommand().
		BPMNProcessId("ssh_cmd").
		LatestVersion().
		VariablesFromMap(map[string]interface{}{
			kHostname: "10.123.234.104:22",
			kUsername: "root",
			kPassword: "default",
		})
	if err != nil {
		panic(fmt.Errorf("failed to create process instance command"))
	}

	ctx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFn()

	process, err := command.Send(ctx)
	if err != nil {
		panic(err)
	}

	log.Printf("started process instance [%d]", process.GetProcessInstanceKey())
	return process
}

func MustStartWorker(client zbc.Client) worker.JobWorker {
	w := client.NewJobWorker().
		JobType(kJobType).
		Handler(HandleJob).
		Concurrency(1).
		MaxJobsActive(10).
		RequestTimeout(1 * time.Second).
		PollInterval(1 * time.Second).
		Name(kWorkerName).
		Open()

	log.Printf("started worker [%s] for jobs of type [%s]", kWorkerName, kJobType)
	return w
}

func HandleJob(client worker.JobClient, job entities.Job) {
	vars, err := job.GetVariablesAsMap()
	if err != nil {
		log.Printf("failed to get variables for job %d: [%s]", job.Key, err)
		return
	}

	log.Printf("all vars: %v", vars)

	resp, err := ExecuteCommand(
		vars[kHostname].(string),
		vars[kUsername].(string),
		vars[kPassword].(string),
		vars[kCommand].(string))

	if err != nil {
		log.Printf("failed to execute command '%s': %s", vars[kCommand].(string), err.Error())
		return
	}

	log.Printf("response: %s", strings.Trim(resp, "\n"))

	ctx, cancelFn := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelFn()

	_, err = client.NewCompleteJobCommand().JobKey(job.Key).Send(ctx)
	if err != nil {
		log.Printf("failed to complete job with key %d: [%s]", job.Key, err)
	}

	log.Printf("completed job %d successfully", job.Key)
}

func ExecuteCommand(hostname, username, password, command string) (string, error) {
	sshConfig := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	connection, err := ssh.Dial("tcp", hostname, sshConfig)
	if err != nil {
		return "", fmt.Errorf("failed to dial: %s", err)
	}

	session, err := connection.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %s", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}

	if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
		session.Close()
		return "", fmt.Errorf("request for pseudo terminal failed: %s", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("unable to setup stdout for session: %v", err)
	}

	buffread := ""

	done := make(chan bool, 1)

	go func() {
		buf := make([]byte, 1000)
		for {
			n, err := stdout.Read(buf)
			if err != nil && err != io.EOF {
				buffread = err.Error()
				done <- true
				break
			}
			if err == io.EOF || n == 0 {
				done <- true
				break
			}
			buffread += string(buf)
		}

	}()

	err = session.Run(command)

	<-done
	return buffread, err

}

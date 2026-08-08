package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type options struct {
	identity   string
	knownHosts string
	host       string
	user       string
	port       string
	command    string
	probe      bool
}

func main() {
	parsed, err := parse(os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(255)
	}
	if parsed.probe {
		return
	}
	if err := run(parsed); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		if exit, ok := err.(*ssh.ExitError); ok {
			os.Exit(exit.ExitStatus())
		}
		os.Exit(255)
	}
}

func parse(arguments []string) (options, error) {
	parsed := options{port: "22"}
	index := 0
	for index < len(arguments) {
		argument := arguments[index]
		switch argument {
		case "-G":
			parsed.probe = true
			index++
		case "-F":
			if index+1 >= len(arguments) {
				return parsed, fmt.Errorf("test SSH: -F requires a value")
			}
			index += 2
		case "-i":
			if index+1 >= len(arguments) {
				return parsed, fmt.Errorf("test SSH: -i requires a value")
			}
			parsed.identity = arguments[index+1]
			index += 2
		case "-p":
			if index+1 >= len(arguments) {
				return parsed, fmt.Errorf("test SSH: -p requires a value")
			}
			parsed.port = arguments[index+1]
			index += 2
		case "-l":
			if index+1 >= len(arguments) {
				return parsed, fmt.Errorf("test SSH: -l requires a value")
			}
			parsed.user = arguments[index+1]
			index += 2
		case "-o":
			if index+1 >= len(arguments) {
				return parsed, fmt.Errorf("test SSH: -o requires a value")
			}
			key, value, _ := strings.Cut(arguments[index+1], "=")
			if strings.EqualFold(key, "UserKnownHostsFile") {
				parsed.knownHosts = strings.Trim(value, `"`)
			}
			index += 2
		case "-4", "-6", "-T":
			index++
		default:
			if strings.HasPrefix(argument, "-") {
				return parsed, fmt.Errorf("test SSH: unsupported option %s", argument)
			}
			target := argument
			if separator := strings.LastIndex(target, "@"); separator >= 0 {
				parsed.user = target[:separator]
				target = target[separator+1:]
			}
			parsed.host = strings.Trim(target, "[]")
			parsed.command = strings.Join(arguments[index+1:], " ")
			index = len(arguments)
		}
	}
	if parsed.probe {
		return parsed, nil
	}
	if parsed.identity == "" || parsed.knownHosts == "" || parsed.host == "" || parsed.user == "" || parsed.command == "" {
		return parsed, fmt.Errorf("test SSH: identity, known_hosts, user, host, and command are required")
	}
	return parsed, nil
}

func run(parsed options) error {
	privateKey, err := os.ReadFile(parsed.identity)
	if err != nil {
		return fmt.Errorf("test SSH: read identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("test SSH: parse identity: %w", err)
	}
	hostKeyCallback, err := knownhosts.New(parsed.knownHosts)
	if err != nil {
		return fmt.Errorf("test SSH: load known_hosts: %w", err)
	}
	address := net.JoinHostPort(parsed.host, parsed.port)
	connection, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		return fmt.Errorf("test SSH: connect: %w", err)
	}
	defer func() { _ = connection.Close() }()
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User:            parsed.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("test SSH: handshake: %w", err)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("test SSH: session: %w", err)
	}
	defer func() { _ = session.Close() }()
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr
	if protocol := os.Getenv("GIT_PROTOCOL"); protocol != "" {
		_ = session.Setenv("GIT_PROTOCOL", protocol)
	}
	return session.Run(parsed.command)
}

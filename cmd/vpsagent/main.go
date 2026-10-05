package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/vpsmon/vpsagent/internal/update"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vpsmon/vpsagent/internal/cloud"
	"github.com/vpsmon/vpsmonlib/metrics"
)

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func main() {
	if len(os.Args) == 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var err error
		switch os.Args[1] {
		case "version":
			fmt.Println(update.Version)
			return
		case "enable-remote-updates":
			err = update.Enable(ctx)
		case "disable-remote-updates":
			err = update.Disable(ctx)
		case "apply-update":
			err = update.ApplyPending(ctx)
		default:
			goto normal
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}
normal:
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		connect(os.Args[2:])
		return
	}
	if len(os.Args) > 1 {
		log.Fatalf("unknown command %q; use 'vpsagent connect' or run without arguments", os.Args[1])
	}

	cloudURL := os.Getenv("VPSAGENT_CLOUD_URL")
	cloudToken := os.Getenv("VPSAGENT_CLOUD_TOKEN")
	if path := os.Getenv("VPSAGENT_CONFIG"); path != "" {
		credentials, err := cloud.LoadCredentials(path)
		if err != nil {
			log.Fatalf("load VPSAGENT_CONFIG: %v", err)
		}
		cloudURL, cloudToken = credentials.URL, credentials.Token
	}
	snapshots := envBool("VPSAGENT_INCIDENT_SNAPSHOTS")
	client, err := cloud.New(cloud.Config{
		URL: cloudURL, Token: cloudToken,
		AllowInsecure:   envBool("VPSAGENT_ALLOW_INSECURE"),
		EnableSnapshots: snapshots,
	})
	if err != nil {
		log.Fatalf("invalid Cloud configuration: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client.StartControl(ctx, update.Version)
	metrics.StartCollectorWithOptions(metrics.Options{TopProcesses: snapshots, Containers: snapshots})
	log.Printf("vpsagent collecting local metrics and uploading to %s", cloudURL)
	client.Start(ctx)
	<-ctx.Done()
}

func connect(args []string) {
	flags := flag.NewFlagSet("vpsagent connect", flag.ExitOnError)
	url := flags.String("url", "", "VPSmon Cloud HTTPS URL")
	setupToken := flags.String("setup-token", "", "short-lived token from VPSmon Cloud")
	setupTokenStdin := flags.Bool("setup-token-stdin", false, "read the setup token from standard input")
	configPath := flags.String("config", "", "owner-only credentials file to create")
	allowInsecure := flags.Bool("allow-insecure-local", false, "allow HTTP only for a localhost development Cloud")
	_ = flags.Parse(args)
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "--config is required; for a service install use /opt/vpsagent/cloud.json")
		os.Exit(2)
	}
	token := *setupToken
	if *setupTokenStdin {
		if token != "" {
			log.Fatal("use either --setup-token or --setup-token-stdin")
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
		if err != nil || len(data) > 4096 {
			log.Fatal("unable to read setup token from standard input")
		}
		token = strings.TrimSpace(string(data))
	}
	if err := cloud.Connect(context.Background(), *url, token, *configPath, *allowInsecure); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Cloud connected. Set VPSAGENT_CONFIG=%s and start vpsagent.\n", *configPath)
}

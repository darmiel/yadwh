package main

import (
	"context"
	"github.com/apex/log"
	"github.com/apex/log/handlers/cli"
	"github.com/docker/docker/client"
	"github.com/gofiber/fiber/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const EnvKeyPrefix = "WH_SECRET_"

var (
	ErrSecretInvalid   = fiber.NewError(401, "secret mismatch")
	ErrWebhookNotFound = fiber.NewError(404, "webhook not found")
)

var secrets = make(map[string]string)

func init() {
	log.SetHandler(cli.Default)
}

func main() {
	// Load secrets from env
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, EnvKeyPrefix) {
			continue
		}
		key := env[:strings.Index(env, "=")]
		name := key[len(EnvKeyPrefix):]
		if len(name) == 0 {
			log.Warnf("Empty secret name: %s", env)
			continue
		}
		value := strings.TrimSpace(os.Getenv(key))
		if len(value) < 12 {
			log.WithField("webhook", name).Warn("Secrets are required to be at least 12 chars long")
			continue
		}
		log.Infof("Found secret for %s = %s", name, strings.Repeat("*", len(value)))
		secrets[name] = value
	}
	if len(secrets) == 0 {
		log.Error("No secrets found.")
		log.Fatalf("Specify them by setting the environment variable to %s<key>=<secret>", EnvKeyPrefix)
	}

	// Docker connection
	log.Info("Connecting to Docker Socket")
	var (
		dc  *client.Client
		err error
	)
	if dc, err = client.NewClientWithOpts(client.FromEnv); err != nil {
		log.WithError(err).Error("Cannot connect to Docker")
		return
	}

	// Test if connection to socket was successful
	if _, err = dc.Info(context.Background()); err != nil {
		log.WithError(err).Fatal("Connection to docker socket failed")
		return
	}

	// Web-Server
	app := fiber.New(fiber.Config{IdleTimeout: 5 * time.Second})

	// secret specified by query, header or body
	app.All("/:name", func(ctx *fiber.Ctx) error {
		name := ctx.Params("name")
		var secret string
		// secret by query
		if secret = ctx.Query("secret"); secret != "" {
			return process(name, secret, ctx)
		}
		if secret = ctx.Get("X-YADWH-Secret"); secret != "" {
			return process(name, secret, ctx)
		}
		if secret = string(ctx.Body()); secret != "" {
			return process(name, secret, ctx)
		}
		return fiber.NewError(401, "secret not found")
	})

	// secret specified in URL
	app.All("/:name/:secret", func(ctx *fiber.Ctx) error {
		return process(ctx.Params("name"), ctx.Params("secret"), ctx)
	})

	sc := make(chan os.Signal)
	signal.Notify(sc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)
	_ = <-sc

	if err = app.Shutdown(); err != nil {
		log.WithError(err).Error("cannot shutdown webserver")
	}
}

func process(name, secret string, ctx *fiber.Ctx) error {
	name = strings.TrimSpace(name)
	secret = strings.TrimSpace(secret)
	// Check if secret is valid
	expected, ok := secrets[name]
	if !ok {
		return ErrWebhookNotFound
	}
	if secret != expected {
		return ErrSecretInvalid
	}

	// TODO: Find all docker containers labeled as {name}

	return ctx.Status(200).SendString("OK")
}

package main

import (
	"context"
	"fmt"
	"github.com/apex/log"
	"github.com/apex/log/handlers/cli"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/gofiber/fiber/v2"
	"github.com/moby/moby/client"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// environment variable prefixes
const (
	EnvSecretPrefix = "WH_SECRET_"
	EnvAuthPrefix   = "WH_AUTH_"
	EnvRemovePrefix = "WH_REMOVE_"
	LabelKey        = "io.d2a.yadwh.ug"
)

// fiber errors
var (
	ErrSecretInvalid   = fiber.NewError(401, "secret mismatch")
	ErrWebhookNotFound = fiber.NewError(404, "webhook not found")
)

// attributes contains label specific settings
type attributes struct {
	secret    string
	auth      string // base64 encoded auth string
	removeOld bool   // remove old image after pulling new
}

var (
	attrs = make(map[string]*attributes)
	dc    *client.Client
)

func init() {
	log.SetHandler(cli.Default)
	log.SetLevel(log.DebugLevel)
}

func main() {
	// Load secrets from env
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, EnvSecretPrefix) {
			continue
		}
		key := env[:strings.Index(env, "=")]
		name := key[len(EnvSecretPrefix):]
		if len(name) == 0 {
			log.Warnf("Empty secret name: %s", env)
			continue
		}

		// find secret in env
		sec := strings.TrimSpace(os.Getenv(key))
		if len(sec) < 12 {
			log.WithField("webhook", name).Warn("Secrets are required to be at least 12 chars long")
			continue
		}
		log.Infof("Found secret for %s = %s", name, strings.Repeat("*", len(sec)))

		// find auth in env
		auth := strings.TrimSpace(os.Getenv(EnvAuthPrefix + name))
		log.Infof("auth secret for %s = %s", name, strings.Repeat("*", len(auth)))

		// find remove old images
		removeOld := strings.TrimSpace(os.Getenv(EnvRemovePrefix+name)) == "true"
		if removeOld { // display warning if purge mode is enabled
			log.Warnf("Purge-Mode was enabled for %s:", name)
			log.Warn("Old images will be deleted after downloading new images.")
		}

		attrs[name] = &attributes{
			secret:    sec,
			auth:      auth,
			removeOld: removeOld,
		}
	}
	if len(attrs) == 0 {
		log.Error("No secrets found.")
		log.Fatalf("Specify them by setting the environment variable to %s<key>=<secret>", EnvSecretPrefix)
		return
	}

	// Docker connection
	log.Info("Connecting to Docker Socket")
	var err error
	if dc, err = client.NewClientWithOpts(client.FromEnv); err != nil {
		log.WithError(err).Error("Cannot connect to Docker")
		return
	}
	log.Debug("Negotiating API version for Docker client")
	dc.NegotiateAPIVersion(context.Background())
	// Test if we can access the docker daemon
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
	go func(s chan os.Signal) {
		if err := app.Listen(":80"); err != nil {
			log.WithError(err).Warn("Cannot listen on port 80")
		}
		sc <- syscall.SIGQUIT // proceed to shut down
	}(sc)

	signal.Notify(sc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)
	_ = <-sc

	log.Info("Shutting down Web-Server")
	if err = app.Shutdown(); err != nil {
		log.WithError(err).Error("cannot shutdown webserver")
	}
}

func isMonitored(watched []string, name string) (monitor bool) {
	for _, w := range watched {
		if strings.EqualFold(strings.TrimSpace(w), name) {
			return true
		}
	}
	return false
}

func trimID(id string) string {
	if len(id) > 16 {
		return id[:15] + "-"
	}
	return id
}

func (a *attributes) pullImage(c *types.Container) (body []byte, err error) {
	log.Infof("Pulling image for container %s@%s", trimID(c.ID), c.Image)
	var reader io.ReadCloser
	defer func() {
		if err = reader.Close(); err != nil {
			log.WithError(err).Warn("Cannot close reader")
		}
	}()
	if reader, err = dc.ImagePull(context.Background(), c.Image, types.ImagePullOptions{
		RegistryAuth: a.auth,
	}); err != nil {
		log.WithError(err).Warn("Cannot pull image")
	}
	body, err = io.ReadAll(reader)
	return
}

func deleteImage(imageID string) (err error) {
	_, err = dc.ImageRemove(context.Background(), imageID, types.ImageRemoveOptions{})
	return
}

func process(name, secret string, ctx *fiber.Ctx) (err error) {
	name = strings.TrimSpace(name)
	secret = strings.TrimSpace(secret)

	// Check if secret is valid
	expected, ok := attrs[name]
	if !ok || expected == nil {
		return ErrWebhookNotFound
	}
	if secret != expected.secret {
		return ErrSecretInvalid
	}

	// Find containers with label
	var containerList []types.Container
	if containerList, err = dc.ContainerList(context.Background(), types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelKey)),
	}); err != nil {
		return fiber.NewError(500, err.Error())
	}

	log.Infof("Finding and restarting containers with label: %s", name)

	// list that contains all restarted containers
	var restarted []types.Container

	for _, cont := range containerList {
		// Check if label contains webhook
		watched := []string{
			cont.Labels[LabelKey],
		}
		if strings.Contains(watched[0], ",") {
			watched = strings.Split(watched[0], ",")
		}

		// check if the container is monitored by this webhook
		if !isMonitored(watched, name) {
			continue
		}

		var body []byte
		if body, err = expected.pullImage(&cont); err != nil {
			continue
		}
		fmt.Println()
		fmt.Println(string(body))
		fmt.Println()

		var inspect types.ContainerJSON
		if inspect, err = dc.ContainerInspect(context.Background(), cont.ID); err != nil {
			log.WithError(err).Warn("Cannot inspect container")
			continue
		}

		// stop container
		log.Infof("Stopping container %s/%s(%s)", cont.ID, cont.Image, cont.ImageID)
		min := time.Minute
		if err = dc.ContainerStop(context.Background(), cont.ID, &min); err != nil {
			log.WithError(err).Warn("Cannot restart container")
			continue
		}

		// remove container
		if !inspect.HostConfig.AutoRemove {
			log.Infof("Removing container %s/%s(%s)", cont.ID, cont.Image, cont.ImageID)
			if err = dc.ContainerRemove(context.Background(), cont.ID, types.ContainerRemoveOptions{}); err != nil {
				log.WithError(err).Warn("Cannot remove container")
				continue
			}
		} else {
			log.Infof("No need to remove container %s/%s(%s)", cont.ID, cont.Image, cont.ImageID)
		}

		// create cont
		containerName := ""
		if len(cont.Names) > 0 {
			containerName = cont.Names[0]
		}

		log.Infof("Re-creating container with image %s", inspect.Config.Image)
		var created container.ContainerCreateCreatedBody
		if created, err = dc.ContainerCreate(context.Background(),
			inspect.Config,
			inspect.HostConfig,
			&network.NetworkingConfig{
				EndpointsConfig: inspect.NetworkSettings.Networks,
			},
			nil,
			containerName,
		); err != nil {
			log.WithError(err).Warn("Cannot create container")
			continue
		}

		log.Infof("Starting container %s", created.ID)
		if err = dc.ContainerStart(context.Background(), created.ID, types.ContainerStartOptions{}); err != nil {
			log.WithError(err).Warn("Cannot start container")
			continue
		}

		// auto delete old image
		if expected.removeOld {
			// quite hacky, is there a better way?
			if strings.Contains(strings.ToLower(string(body)), cont.ImageID) {
				log.Infof("It looks like the old image was pulled again. Skipped removing.")
			} else {
				log.Infof("Deleting image %s", cont.ImageID)
				if err = deleteImage(cont.ImageID); err != nil {
					log.WithError(err).Warn("Cannot remove old image")
				}
			}
		}

		log.Infof("Done! Container with image (%s) updated", cont.Image)
		restarted = append(restarted, cont)
	}

	return ctx.Status(200).JSON(restarted)
}

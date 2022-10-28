package cli

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/rs/zerolog/log"

	v1 "github.com/dyrector-io/dyrectorio/golang/api/v1"
	containerbuilder "github.com/dyrector-io/dyrectorio/golang/pkg/builder/container"
	dagentutils "github.com/dyrector-io/dyrectorio/golang/pkg/dagent/utils"
)

type DyrectorioStack struct {
	Containers     Containers
	Traefik        *containerbuilder.DockerContainerBuilder
	Crux           *containerbuilder.DockerContainerBuilder
	CruxMigrate    *containerbuilder.DockerContainerBuilder
	CruxUI         *containerbuilder.DockerContainerBuilder
	Kratos         *containerbuilder.DockerContainerBuilder
	KratosMigrate  *containerbuilder.DockerContainerBuilder
	CruxPostgres   *containerbuilder.DockerContainerBuilder
	KratosPostgres *containerbuilder.DockerContainerBuilder
	MailSlurper    *containerbuilder.DockerContainerBuilder
}

const (
	ContainerNetDriver = "bridge"
	PodmanHost         = "host.containers.internal"
	DockerHost         = "host.docker.internal"
)

const (
	UpCommand   = "up"
	DownCommand = "down"
)

type traefikFileProviderData struct {
	Service string
	Port    uint
	Host    string
}

//go:embed traefik.yaml.tmpl
var traefikTmpl embed.FS

func ProcessCommand(settings *Settings) {
	containers := DyrectorioStack{
		Containers: settings.Containers,
	}
	switch settings.Command {
	case UpCommand:
		PrintInfo(settings)
		settings = CheckAndUpdatePorts(settings)
		SaveSettings(settings)

		containers.Traefik = GetTraefik(settings)
		containers.Crux = GetCrux(settings)
		containers.CruxMigrate = GetCruxMigrate(settings)
		containers.CruxUI = GetCruxUI(settings)
		containers.Kratos = GetKratos(settings)
		containers.KratosMigrate = GetKratosMigrate(settings)
		containers.CruxPostgres = GetCruxPostgres(settings)
		containers.KratosPostgres = GetKratosPostgres(settings)
		containers.MailSlurper = GetMailSlurper(settings)

		StartContainers(&containers, settings.InternalHostDomain)
	case DownCommand:
		StopContainers(&containers)
	default:
		log.Fatal().Msg("invalid command")
	}
}

// Create and Start containers
func StartContainers(containers *DyrectorioStack, internalHostDomain string) {
	_, err := containers.Traefik.Create().Start()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}
	TraefikConfiguration(
		containers.Containers.Traefik.Name,
		internalHostDomain,
		containers.Containers.CruxUI.CruxUIPort,
	)

	_, err = containers.CruxPostgres.Create().Start()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	_, err = containers.KratosPostgres.Create().Start()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	log.Printf("Migration (kratos) in progress...")
	_, err = containers.KratosMigrate.Create().StartWaitUntilExit()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}
	log.Printf("Migration (kratos) done!")

	_, err = containers.Kratos.Create().Start()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	if !containers.Containers.Crux.Disabled {
		log.Printf("Migration (crux) in progress...")
		_, err = containers.CruxMigrate.Create().StartWaitUntilExit()
		if err != nil {
			log.Fatal().Err(err).Stack().Msg("")
		}
		log.Printf("Migration (crux) done!")

		_, err = containers.Crux.Create().Start()
		if err != nil {
			log.Fatal().Err(err).Stack().Msg("")
		}
	}

	if !containers.Containers.CruxUI.Disabled {
		_, err = containers.CruxUI.Create().Start()
		if err != nil {
			log.Fatal().Err(err).Stack().Msg("")
		}
	}

	_, err = containers.MailSlurper.Create().Start()
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}
}

// Cleanup for "down" command
func StopContainers(containers *DyrectorioStack) {
	if containerID := containers.Containers.MailSlurper.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}

	if !containers.Containers.CruxUI.Disabled {
		if containerID := containers.Containers.CruxUI.Name; GetContainerID(containerID) != "" {
			CleanupContainer(containerID)
		}
	}

	if !containers.Containers.Crux.Disabled {
		if containerID := containers.Containers.Crux.Name; GetContainerID(containerID) != "" {
			CleanupContainer(containerID)
		}

		if containerID := containers.Containers.CruxMigrate.Name; GetContainerID(containerID) != "" {
			CleanupContainer(containerID)
		}
	}

	if containerID := containers.Containers.KratosMigrate.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}

	if containerID := containers.Containers.Kratos.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}

	if containerID := containers.Containers.CruxPostgres.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}

	if containerID := containers.Containers.KratosPostgres.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}

	if containerID := containers.Containers.Traefik.Name; GetContainerID(containerID) != "" {
		CleanupContainer(containerID)
	}
}

// Copy to Traefik Container
func TraefikConfiguration(name, internalHostDomain string, cruxuiport uint) {
	const funct = "TraefikConfiguration"
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Stack().Msg(funct)
	}

	traefikFileProviderTemplate, err := traefikTmpl.ReadFile("traefik.yaml.tmpl")
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("couldn't read embedded file")
	}

	traefikConfig, err := template.New("traefikconfig").Parse(string(traefikFileProviderTemplate))
	if err != nil {
		log.Fatal().Err(err).Stack().Msg(funct)
	}

	var result bytes.Buffer

	traefikData := traefikFileProviderData{
		Service: "crux-ui",
		Port:    cruxuiport,
		Host:    internalHostDomain,
	}

	err = traefikConfig.Execute(&result, traefikData)
	if err != nil {
		log.Fatal().Err(err).Stack().Msg(funct)
	}

	data := v1.UploadFileData{
		FilePath: "/etc",
		UID:      0,
		GID:      0,
	}

	err = dagentutils.WriteContainerFile(
		context.Background(),
		cli,
		name,
		"traefik.dev.yml",
		data,
		int64(len([]rune(result.String()))),
		strings.NewReader(result.String()),
	)

	if err != nil {
		log.Fatal().Err(err).Stack().Msg(funct)
	}
}

// Helper function to get the container's ID from name
func GetContainerID(name string) string {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	filter := filters.NewArgs()
	filter.Add("name", fmt.Sprintf("^%s$", name))

	containers, err := cli.ContainerList(
		context.Background(),
		types.ContainerListOptions{
			All:     true,
			Filters: filter,
		})
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	switch len(containers) {
	case 0:
		log.Printf("no such container found with name: %s", name)
		return ""
	case 1:
		return containers[0].ID
	default:
		log.Fatal().Msg("ambigous name")
		return ""
	}
}

// Stop and Delete container
func CleanupContainer(id string) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	timeout, err := time.ParseDuration("10s")
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	err = cli.ContainerStop(context.Background(), id, &timeout)
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	err = cli.ContainerRemove(context.Background(), id, types.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}
}

func EnsureNetworkExists(settings *Settings) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	filter := filters.NewArgs()
	filter.Add("name", fmt.Sprintf("^%s$", settings.SettingsFile.Network))

	networks, err := cli.NetworkList(context.Background(),
		types.NetworkListOptions{
			Filters: filter,
		})
	if err != nil {
		log.Fatal().Err(err).Stack().Msg("")
	}

	if len(networks) == 0 {
		opts := types.NetworkCreate{
			Driver: ContainerNetDriver,
		}

		resp, err := cli.NetworkCreate(context.Background(), settings.SettingsFile.Network, opts)
		log.Info().Interface("resp", resp).Msg("")
		if err != nil {
			log.Fatal().Err(err).Stack().Msg("")
		}
		return
	}

	for i := range networks {
		if networks[i].Driver != ContainerNetDriver {
			log.Fatal().
				Str("network", settings.SettingsFile.Network).
				Str("driver", ContainerNetDriver).
				Msg("network exists, but doesn't have the correct driver")
		} else {
			return
		}
	}

	log.Fatal().Msg("unknown network error")
}
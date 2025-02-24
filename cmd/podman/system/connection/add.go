package connection

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/ssh"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/system"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	addCmd = &cobra.Command{
		Use:   "add [options] NAME DESTINATION",
		Args:  cobra.ExactArgs(2),
		Short: "Record destination for the Podman service",
		Long: `Add destination to podman configuration.
  "destination" is one of the form:
    [user@]hostname (will default to ssh)
    ssh://[user@]hostname[:port][/path] (will obtain socket path from service, if not given.)
    tcp://hostname:port (not secured)
    unix://path (absolute path required)
`,
		RunE:              add,
		ValidArgsFunction: completion.AutocompleteNone,
		Example: `podman system connection add laptop server.fubar.com
  podman system connection add --identity ~/.ssh/dev_rsa testing ssh://root@server.fubar.com:2222
  podman system connection add --identity ~/.ssh/dev_rsa --port 22 production root@server.fubar.com
  podman system connection add debug tcp://localhost:8080
  `,
	}

	createCmd = &cobra.Command{
		Use:               "create [options] NAME DESTINATION",
		Args:              cobra.ExactArgs(1),
		Short:             addCmd.Short,
		Long:              addCmd.Long,
		RunE:              create,
		ValidArgsFunction: completion.AutocompleteNone,
	}

	dockerPath string

	cOpts = struct {
		Identity string
		Port     int
		UDSPath  string
		Default  bool
	}{}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: addCmd,
		Parent:  system.ConnectionCmd,
	})
	flags := addCmd.Flags()

	portFlagName := "port"
	flags.IntVarP(&cOpts.Port, portFlagName, "p", 22, "SSH port number for destination")
	_ = addCmd.RegisterFlagCompletionFunc(portFlagName, completion.AutocompleteNone)

	identityFlagName := "identity"
	flags.StringVar(&cOpts.Identity, identityFlagName, "", "path to SSH identity file")
	_ = addCmd.RegisterFlagCompletionFunc(identityFlagName, completion.AutocompleteDefault)

	socketPathFlagName := "socket-path"
	flags.StringVar(&cOpts.UDSPath, socketPathFlagName, "", "path to podman socket on remote host. (default '/run/podman/podman.sock' or '/run/user/{uid}/podman/podman.sock)")
	_ = addCmd.RegisterFlagCompletionFunc(socketPathFlagName, completion.AutocompleteDefault)

	flags.BoolVarP(&cOpts.Default, "default", "d", false, "Set connection to be default")

	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: createCmd,
		Parent:  system.ContextCmd,
	})

	flags = createCmd.Flags()
	dockerFlagName := "docker"
	flags.StringVar(&dockerPath, dockerFlagName, "", "Description of the context")

	_ = createCmd.RegisterFlagCompletionFunc(dockerFlagName, completion.AutocompleteNone)
	flags.String("description", "", "Ignored.  Just for script compatibility")
	flags.String("from", "", "Ignored.  Just for script compatibility")
	flags.String("kubernetes", "", "Ignored.  Just for script compatibility")
	flags.String("default-stack-orchestrator", "", "Ignored.  Just for script compatibility")
}

func add(cmd *cobra.Command, args []string) error {
	// Default to ssh schema if none given

	entities := &ssh.ConnectionCreateOptions{
		Port:     cOpts.Port,
		Path:     args[1],
		Identity: cOpts.Identity,
		Name:     args[0],
		Socket:   cOpts.UDSPath,
		Default:  cOpts.Default,
	}
	dest := args[1]
	if match, err := regexp.MatchString("^[A-Za-z][A-Za-z0-9+.-]*://", dest); err != nil {
		return fmt.Errorf("invalid destination: %w", err)
	} else if !match {
		dest = "ssh://" + dest
	}
	uri, err := url.Parse(dest)
	if err != nil {
		return err
	}

	if cmd.Flags().Changed("socket-path") {
		uri.Path = cmd.Flag("socket-path").Value.String()
	}

	var sshMode ssh.EngineMode
	containerConfig := registry.PodmanConfig()

	flag := containerConfig.SSHMode

	sshMode = ssh.DefineMode(flag)

	if sshMode == ssh.InvalidMode {
		return fmt.Errorf("invalid ssh mode")
	}

	switch uri.Scheme {
	case "ssh":
		return ssh.Create(entities, sshMode)
	case "unix":
		if cmd.Flags().Changed("identity") {
			return errors.New("--identity option not supported for unix scheme")
		}

		if cmd.Flags().Changed("socket-path") {
			uri.Path = cmd.Flag("socket-path").Value.String()
		}

		info, err := os.Stat(uri.Path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			logrus.Warnf("%q does not exist", uri.Path)
		case errors.Is(err, os.ErrPermission):
			logrus.Warnf("You do not have permission to read %q", uri.Path)
		case err != nil:
			return err
		case info.Mode()&os.ModeSocket == 0:
			return fmt.Errorf("%q exists and is not a unix domain socket", uri.Path)
		}
	case "tcp":
		if cmd.Flags().Changed("socket-path") {
			return errors.New("--socket-path option not supported for tcp scheme")
		}
		if cmd.Flags().Changed("identity") {
			return errors.New("--identity option not supported for tcp scheme")
		}
		if uri.Port() == "" {
			return errors.New("tcp scheme requires a port either via --port or in destination URL")
		}
	default:
		logrus.Warnf("%q unknown scheme, no validation provided", uri.Scheme)
	}

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	if cmd.Flags().Changed("default") {
		if cOpts.Default {
			cfg.Engine.ActiveService = args[0]
		}
	}

	dst := config.Destination{
		URI: uri.String(),
	}

	if cmd.Flags().Changed("identity") {
		dst.Identity = cOpts.Identity
	}

	if cfg.Engine.ServiceDestinations == nil {
		cfg.Engine.ServiceDestinations = map[string]config.Destination{
			args[0]: dst,
		}
		cfg.Engine.ActiveService = args[0]
	} else {
		cfg.Engine.ServiceDestinations[args[0]] = dst
	}
	return cfg.Write()
}

func create(cmd *cobra.Command, args []string) error {
	dest, err := translateDest(dockerPath)
	if err != nil {
		return err
	}
	if match, err := regexp.MatchString("^[A-Za-z][A-Za-z0-9+.-]*://", dest); err != nil {
		return fmt.Errorf("invalid destination: %w", err)
	} else if !match {
		dest = "ssh://" + dest
	}

	uri, err := url.Parse(dest)
	if err != nil {
		return err
	}

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	dst := config.Destination{
		URI: uri.String(),
	}

	if cfg.Engine.ServiceDestinations == nil {
		cfg.Engine.ServiceDestinations = map[string]config.Destination{
			args[0]: dst,
		}
		cfg.Engine.ActiveService = args[0]
	} else {
		cfg.Engine.ServiceDestinations[args[0]] = dst
	}
	return cfg.Write()
}

func translateDest(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	split := strings.SplitN(path, "=", 2)
	if len(split) == 1 {
		return split[0], nil
	}
	if split[0] != "host" {
		return "", fmt.Errorf("\"host\" is requited for --docker option")
	}
	// "host=tcp://myserver:2376,ca=~/ca-file,cert=~/cert-file,key=~/key-file"
	vals := strings.Split(split[1], ",")
	if len(vals) > 1 {
		return "", fmt.Errorf("--docker additional options %q not supported", strings.Join(vals[1:], ","))
	}
	// for now we ignore other fields specified on command line
	return vals[0], nil
}

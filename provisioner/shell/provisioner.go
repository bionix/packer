// This package implements a provisioner for Packer that executes
// shell scripts within the remote machine.
package shell

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/mitchellh/mapstructure"
	"github.com/mitchellh/packer/packer"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
	"text/template"
)

const DefaultRemotePath = "/tmp/script.sh"

type config struct {
	// An inline script to execute. Multiple strings are all executed
	// in the context of a single shell.
	Inline []string

	// The shebang value used when running inline scripts.
	InlineShebang string `mapstructure:"inline_shebang"`

	// The local path of the shell script to upload and execute.
	Script string

	// An array of multiple scripts to run.
	Scripts []string

	// An array of environment variables that will be injected before
	// your command(s) are executed.
	Vars []string `mapstructure:"environment_vars"`

	// The remote path where the local shell script will be uploaded to.
	// This should be set to a writable file that is in a pre-existing directory.
	RemotePath string `mapstructure:"remote_path"`

	// The command used to execute the script. The '{{ .Path }}' variable
	// should be used to specify where the script goes, {{ .Vars }}
	// can be used to inject the environment_vars into the environment.
	ExecuteCommand string `mapstructure:"execute_command"`

	// Packer configurations, these come from Packer itself
	PackerBuildName   string `mapstructure:"packer_build_name"`
	PackerBuilderType string `mapstructure:"packer_builder_type"`
}

type Provisioner struct {
	config config
}

type ExecuteCommandTemplate struct {
	Vars string
	Path string
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	var md mapstructure.Metadata
	decoderConfig := &mapstructure.DecoderConfig{
		Metadata: &md,
		Result:   &p.config,
	}

	decoder, err := mapstructure.NewDecoder(decoderConfig)
	if err != nil {
		return err
	}

	for _, raw := range raws {
		err := decoder.Decode(raw)
		if err != nil {
			return err
		}
	}

	// Accumulate any errors
	errs := make([]error, 0)

	// Unused keys are errors
	if len(md.Unused) > 0 {
		sort.Strings(md.Unused)
		for _, unused := range md.Unused {
			if unused != "type" && !strings.HasPrefix(unused, "packer_") {
				errs = append(
					errs, fmt.Errorf("Unknown configuration key: %s", unused))
			}
		}
	}

	if p.config.ExecuteCommand == "" {
		p.config.ExecuteCommand = "chmod +x {{.Path}}; {{.Vars}} {{.Path}}"
	}

	if p.config.Inline != nil && len(p.config.Inline) == 0 {
		p.config.Inline = nil
	}

	if p.config.InlineShebang == "" {
		p.config.InlineShebang = "/bin/sh"
	}

	if p.config.RemotePath == "" {
		p.config.RemotePath = DefaultRemotePath
	}

	if p.config.Scripts == nil {
		p.config.Scripts = make([]string, 0)
	}

	if p.config.Vars == nil {
		p.config.Vars = make([]string, 0)
	}

	if p.config.Script != "" && len(p.config.Scripts) > 0 {
		errs = append(errs, errors.New("Only one of script or scripts can be specified."))
	}

	if p.config.Script != "" {
		p.config.Scripts = []string{p.config.Script}
	}

	if len(p.config.Scripts) == 0 && p.config.Inline == nil {
		errs = append(errs, errors.New("Either a script file or inline script must be specified."))
	} else if len(p.config.Scripts) > 0 && p.config.Inline != nil {
		errs = append(errs, errors.New("Only a script file or an inline script can be specified, not both."))
	}

	for _, path := range p.config.Scripts {
		if _, err := os.Stat(path); err != nil {
			errs = append(errs, fmt.Errorf("Bad script '%s': %s", path, err))
		}
	}

	// Do a check for bad environment variables, such as '=foo', 'foobar'
	for _, kv := range p.config.Vars {
		vs := strings.Split(kv, "=")
		if len(vs) != 2 || vs[0] == "" {
			errs = append(
				errs,
				fmt.Errorf("Environment variable not in format 'key=value': %s", kv))
		}
	}

	if len(errs) > 0 {
		return &packer.MultiError{errs}
	}

	return nil
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	scripts := make([]string, len(p.config.Scripts))
	copy(scripts, p.config.Scripts)

	// If we have an inline script, then turn that into a temporary
	// shell script and use that.
	if p.config.Inline != nil {
		tf, err := ioutil.TempFile("", "packer-shell")
		if err != nil {
			return fmt.Errorf("Error preparing shell script: %s", err)
		}
		defer os.Remove(tf.Name())

		// Set the path to the temporary file
		scripts = append(scripts, tf.Name())

		// Write our contents to it
		writer := bufio.NewWriter(tf)
		writer.WriteString(fmt.Sprintf("#!%s\n", p.config.InlineShebang))
		for _, command := range p.config.Inline {
			if _, err := writer.WriteString(command + "\n"); err != nil {
				return fmt.Errorf("Error preparing shell script: %s", err)
			}
		}

		if err := writer.Flush(); err != nil {
			return fmt.Errorf("Error preparing shell script: %s", err)
		}

		tf.Close()
	}

	// Build our variables up by adding in the build name and builder type
	envVars := make([]string, len(p.config.Vars)+2)
	envVars[0] = "PACKER_BUILD_NAME=" + p.config.PackerBuildName
	envVars[1] = "PACKER_BUILDER_TYPE=" + p.config.PackerBuilderType
	copy(envVars[2:], p.config.Vars)

	for _, path := range scripts {
		ui.Say(fmt.Sprintf("Provisioning with shell script: %s", path))

		log.Printf("Opening %s for reading", path)
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("Error opening shell script: %s", err)
		}
		defer f.Close()

		log.Printf("Uploading %s => %s", path, p.config.RemotePath)
		err = comm.Upload(p.config.RemotePath, f)
		if err != nil {
			return fmt.Errorf("Error uploading shell script: %s", err)
		}

		// Close the original file since we copied it
		f.Close()

		// Flatten the environment variables
		flattendVars := strings.Join(envVars, " ")

		// Compile the command
		var command bytes.Buffer
		t := template.Must(template.New("command").Parse(p.config.ExecuteCommand))
		t.Execute(&command, &ExecuteCommandTemplate{flattendVars, p.config.RemotePath})

		cmd := &packer.RemoteCmd{Command: command.String()}
		log.Printf("Executing command: %s", cmd.Command)
		if err := cmd.StartWithUi(comm, ui); err != nil {
			return fmt.Errorf("Failed executing command: %s", err)
		}

		if cmd.ExitStatus != 0 {
			return fmt.Errorf("Script exited with non-zero exit status: %d", cmd.ExitStatus)
		}
	}

	return nil
}

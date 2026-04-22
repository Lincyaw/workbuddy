package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBuildCLISchemaUsesStableMaps(t *testing.T) {
	t.Parallel()

	root := &cobra.Command{
		Use:   "root",
		Short: "root command",
	}
	root.Flags().String("zeta", "last", "")
	root.Flags().String("alpha", "first", "")

	alphaCmd := &cobra.Command{
		Use:  "alpha <value>",
		Long: "alpha long description\nwith more details",
		Run: func(cmd *cobra.Command, args []string) {
		},
	}
	alphaCmd.Flags().String("required", "", "")
	if err := alphaCmd.MarkFlagRequired("required"); err != nil {
		t.Fatalf("mark required flag: %v", err)
	}

	betaCmd := &cobra.Command{
		Use:   "beta",
		Short: "beta command",
		Run: func(cmd *cobra.Command, args []string) {
		},
	}
	betaCmd.Flags().Bool("enabled", true, "")

	root.AddCommand(betaCmd, alphaCmd)

	schema, err := buildCLISchema(root)
	if err != nil {
		t.Fatalf("build cli schema: %v", err)
	}

	if got, want := schema.Command.Flags["alpha"].Default, "first"; got != want {
		t.Fatalf("alpha default = %#v, want %#v", got, want)
	}
	if got := schema.Command.Subcommands["alpha"].Short; got != "alpha long description" {
		t.Fatalf("alpha short = %q, want first long line", got)
	}
	if !schema.Command.Subcommands["alpha"].Flags["required"].Required {
		t.Fatal("required flag not marked required in schema")
	}

	payload, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}

	if !bytes.Contains(payload, []byte(`"flags":{"alpha"`)) {
		t.Fatalf("flags are not encoded as a stable object map: %s", payload)
	}
	if !bytes.Contains(payload, []byte(`"subcommands":{"alpha"`)) {
		t.Fatalf("subcommands are not encoded as a stable object map: %s", payload)
	}
}

func TestDumpSchemaEmitsJSONAndContainsCoreCommands(t *testing.T) {
	t.Parallel()

	stdout, stderr := runCLIHelper(t, "--dump-schema")
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var schema cliSchema
	if err := json.Unmarshal([]byte(stdout), &schema); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}

	if schema.SchemaVersion != cliSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schema.SchemaVersion, cliSchemaVersion)
	}

	for _, name := range []string{"deploy", "coordinator", "worker"} {
		if _, ok := schema.Command.Subcommands[name]; !ok {
			t.Fatalf("schema missing %q command", name)
		}
	}
}

func TestVersionFlagPrintsVersionString(t *testing.T) {
	t.Parallel()

	stdout, stderr := runCLIHelper(t, "--version")
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	if got, want := stdout, versionString()+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_WORKBUDDY_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		_, _ = os.Stderr.WriteString("missing argument separator\n")
		os.Exit(2)
	}

	rootCmd.SetArgs(args[sep+1:])
	rootCmd.SetOut(os.Stdout)
	rootCmd.SetErr(os.Stderr)

	if err := rootCmd.Execute(); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}

	os.Exit(0)
}

func runCLIHelper(t *testing.T, args ...string) (string, string) {
	t.Helper()

	cmdArgs := append([]string{"-test.run=TestCLIHelperProcess", "--"}, args...)
	helper := exec.Command(os.Args[0], cmdArgs...)
	helper.Env = append(os.Environ(), "GO_WANT_WORKBUDDY_HELPER_PROCESS=1")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	helper.Stdout = &stdout
	helper.Stderr = &stderr

	if err := helper.Run(); err != nil {
		t.Fatalf("run helper %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}

	return stdout.String(), strings.TrimSpace(stderr.String())
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"runtime/trace"
	"strings"

	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/google/subcommands"
	"github.com/nelhage/llama/cmd/internal/cli"
	"github.com/nelhage/llama/llama"
	"github.com/nelhage/llama/protocol"
	"github.com/nelhage/llama/store"
)

type inputFile struct {
	source string
	dest   string
}

type fileList struct {
	files []inputFile
}

func (f *fileList) String() string {
	return ""
}

func (f *fileList) Get() interface{} {
	return f.files
}

func (f *fileList) Set(v string) error {
	idx := strings.IndexRune(v, ':')
	var source, dest string
	if idx > 0 {
		source = v[:idx]
		dest = v[idx+1:]
	} else {
		source = v
		dest = v
	}
	if path.IsAbs(dest) {
		return fmt.Errorf("-file: cannot expose file at absolute path: %q", dest)
	}
	f.files = append(f.files, inputFile{source, dest})
	return nil
}

func (f *fileList) Prepare(ctx context.Context, store store.Store) (map[string]protocol.File, error) {
	if f.files == nil {
		return nil, nil
	}
	var outErr error
	files := make(map[string]protocol.File, len(f.files))
	trace.WithRegion(ctx, "uploadFiles", func() {
		for _, file := range f.files {
			data, err := ioutil.ReadFile(file.source)
			if err != nil {
				outErr = fmt.Errorf("reading file %q: %w", file.source, err)
				return
			}
			st, err := os.Stat(file.source)
			if err != nil {
				outErr = fmt.Errorf("stat %q: %w", file.source, err)
				return
			}
			blob, err := protocol.NewBlob(ctx, store, data)
			if err != nil {
				outErr = err
				return
			}
			files[file.dest] = protocol.File{Blob: *blob, Mode: st.Mode()}
		}
	})
	if outErr != nil {
		return nil, outErr
	}
	return files, nil
}

type InvokeCommand struct {
	stdin bool
	logs  bool
	files fileList
}

func (*InvokeCommand) Name() string     { return "invoke" }
func (*InvokeCommand) Synopsis() string { return "Invoke a llama command" }
func (*InvokeCommand) Usage() string {
	return `invoke FUNCTION-NAME ARGS...
`
}

func (c *InvokeCommand) SetFlags(flags *flag.FlagSet) {
	flags.BoolVar(&c.stdin, "stdin", false, "Read from stdin and pass it to the command")
	flags.BoolVar(&c.logs, "logs", false, "Display command invocation logs")
	flags.Var(&c.files, "f", "Pass a file through to the invocation")
	flags.Var(&c.files, "file", "Pass a file through to the invocation")
}

func (c *InvokeCommand) Execute(ctx context.Context, flag *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	global := cli.MustState(ctx)

	var spec protocol.InvocationSpec

	if c.stdin {
		stdin, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			log.Printf("reading stdin: %s", err.Error())
			return subcommands.ExitFailure
		}
		spec.Stdin, err = protocol.NewBlob(ctx, global.Store, stdin)
		if err != nil {
			log.Printf("writing to store: %s", err.Error())
			return subcommands.ExitFailure
		}
	}

	var err error
	if len(c.files.files) > 0 {
		spec.Files, err = c.files.Prepare(ctx, global.Store)
		if err != nil {
			log.Println(err.Error())
			return subcommands.ExitFailure
		}
	}

	var outputs map[string]string
	trace.WithRegion(ctx, "prepareArguments", func() {
		spec.Args, outputs, err = prepareArgs(ctx, global, flag.Args()[1:])
	})
	if err != nil {
		log.Println("preparing arguments: ", err.Error())
		return subcommands.ExitFailure
	}

	svc := lambda.New(global.Session)
	response, err := llama.Invoke(ctx, svc, &llama.InvokeArgs{
		Function:   flag.Arg(0),
		Spec:       spec,
		ReturnLogs: c.logs,
	})
	if err != nil {
		if ir, ok := err.(*llama.ErrorReturn); ok {
			if ir.Logs != nil {
				fmt.Fprintf(os.Stderr, "==== invocation logs ====\n%s\n==== end logs ====\n", ir.Logs)
			}
		}
		log.Fatalf("invoke: %s", err.Error())
	}

	if response.Logs != nil {
		fmt.Fprintf(os.Stderr, "==== invocation logs ====\n%s\n==== end logs ====\n", response.Logs)
	}

	fetchOutputs(ctx, outputs, &response.Response)

	if response.Response.Stderr != nil {
		bytes, err := response.Response.Stderr.Read(ctx, global.Store)
		if err != nil {
			log.Printf("Reading stderr: %s", err.Error())
		} else {
			os.Stderr.Write(bytes)
		}
	}
	if response.Response.Stdout != nil {
		bytes, err := response.Response.Stdout.Read(ctx, global.Store)
		if err != nil {
			log.Printf("Reading stdout: %s", err.Error())
		} else {
			os.Stdout.Write(bytes)
		}
	}

	return subcommands.ExitStatus(response.Response.ExitStatus)
}

func parseArg(ctx context.Context, outputs *map[string]string, arg string) (json.RawMessage, error) {
	global := cli.MustState(ctx)
	var argSpec interface{} = arg
	idx := strings.Index(arg, "@")
	if idx > 0 {
		pfx := arg[:idx]
		arg = arg[idx+1:]

		var a protocol.Arg
		switch pfx {
		case "i", "io":
			data, err := ioutil.ReadFile(arg)
			if err != nil {
				return nil, fmt.Errorf("Reading file: %q: %w", arg, err)

			}
			a.In, err = protocol.NewBlob(ctx, global.Store, data)
			if err != nil {
				return nil, fmt.Errorf("Writing to store: %q: %w", arg, err)
			}
			argSpec = a
			if pfx == "i" {
				break
			}
			fallthrough
		case "o":
			name := path.Base(arg)
			if *outputs != nil {
				if _, ok := (*outputs)[name]; ok {
					name = fmt.Sprintf("%d-%s", len(*outputs), name)
				}
			}

			a.Out = &name
			argSpec = a
			if *outputs == nil {
				*outputs = make(map[string]string)
			}
			(*outputs)[name] = arg
		case "raw":
			argSpec = arg
		default:
			return nil, fmt.Errorf("Unrecognize argspec: %s@...", pfx)
		}
	}
	word, err := json.Marshal(argSpec)
	if err != nil {
		log.Fatal("marshal: ", err)
	}
	return word, nil
}

func fetchOutputs(ctx context.Context, outputs map[string]string, resp *protocol.InvocationResponse) {
	trace.WithRegion(ctx, "fetchOutputs", func() {
		global := cli.MustState(ctx)
		for key, blob := range resp.Outputs {
			file, ok := outputs[key]
			if !ok {
				log.Printf("Unexpected output: %q", key)
				continue
			}
			data, err := blob.Read(ctx, global.Store)
			if err != nil {
				log.Printf("reading output %q: %s", key, err.Error())
				continue
			}
			if err := ioutil.WriteFile(file, data, 0644); err != nil {
				log.Printf("reading output %q: %s", file, err.Error())
			}
		}
	})
}

func prepareArgs(ctx context.Context, global *cli.GlobalState, args []string) ([]json.RawMessage, map[string]string, error) {
	out := make([]json.RawMessage, len(args))
	var outputs map[string]string
	for i, arg := range args {
		var err error
		out[i], err = parseArg(ctx, &outputs, arg)
		if err != nil {
			return nil, nil, err
		}
	}
	return out, outputs, nil
}

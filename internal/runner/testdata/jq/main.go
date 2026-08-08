package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/itchyny/gojq"
)

type options struct {
	nullInput bool
	rawOutput bool
	compact   bool
	exitCode  bool
	variables []string
	values    []any
	filter    string
	files     []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(arguments []string, input io.Reader, output, errorsOutput io.Writer) int {
	options, err := parse(arguments)
	if err != nil {
		_, _ = fmt.Fprintln(errorsOutput, err)
		return 2
	}
	query, err := gojq.Parse(options.filter)
	if err != nil {
		_, _ = fmt.Fprintln(errorsOutput, err)
		return 3
	}
	code, err := gojq.Compile(query, gojq.WithVariables(options.variables))
	if err != nil {
		_, _ = fmt.Fprintln(errorsOutput, err)
		return 3
	}

	inputs, err := readInputs(options, input)
	if err != nil {
		_, _ = fmt.Fprintln(errorsOutput, err)
		return 2
	}
	produced := false
	var last any
	for _, value := range inputs {
		iterator := code.Run(value, options.values...)
		for {
			result, ok := iterator.Next()
			if !ok {
				break
			}
			if queryErr, ok := result.(error); ok {
				_, _ = fmt.Fprintln(errorsOutput, queryErr)
				return 5
			}
			if err := writeValue(output, result, options); err != nil {
				_, _ = fmt.Fprintln(errorsOutput, err)
				return 2
			}
			produced = true
			last = result
		}
	}
	if !options.exitCode {
		return 0
	}
	if !produced {
		return 4
	}
	if last == nil || last == false {
		return 1
	}
	return 0
}

func parse(arguments []string) (options, error) {
	var parsed options
	index := 0
	for index < len(arguments) {
		argument := arguments[index]
		if argument == "--" {
			index++
			break
		}
		if argument == "--arg" || argument == "--argjson" {
			if index+2 >= len(arguments) {
				return parsed, fmt.Errorf("jq test helper: %s requires a name and value", argument)
			}
			name := "$" + arguments[index+1]
			var value any = arguments[index+2]
			if argument == "--argjson" {
				if err := json.Unmarshal([]byte(arguments[index+2]), &value); err != nil {
					return parsed, fmt.Errorf("jq test helper: decode --argjson %s: %w", name, err)
				}
			}
			parsed.variables = append(parsed.variables, name)
			parsed.values = append(parsed.values, value)
			index += 3
			continue
		}
		if strings.HasPrefix(argument, "-") && argument != "-" {
			for _, option := range strings.TrimPrefix(argument, "-") {
				switch option {
				case 'n':
					parsed.nullInput = true
				case 'r':
					parsed.rawOutput = true
				case 'c':
					parsed.compact = true
				case 'e':
					parsed.exitCode = true
				case 'M', 'S':
					// Color and key ordering do not alter these script contracts.
				default:
					return parsed, fmt.Errorf("jq test helper: unsupported option -%c", option)
				}
			}
			index++
			continue
		}
		break
	}
	if index >= len(arguments) {
		return parsed, fmt.Errorf("jq test helper: filter is required")
	}
	parsed.filter = arguments[index]
	parsed.files = append(parsed.files, arguments[index+1:]...)
	return parsed, nil
}

func readInputs(options options, input io.Reader) ([]any, error) {
	if options.nullInput {
		return []any{nil}, nil
	}
	readers := []io.Reader{input}
	closers := make([]io.Closer, 0, len(options.files))
	if len(options.files) > 0 {
		readers = readers[:0]
		for _, path := range options.files {
			file, err := os.Open(path)
			if err != nil {
				for _, closer := range closers {
					_ = closer.Close()
				}
				return nil, err
			}
			readers = append(readers, file)
			closers = append(closers, file)
		}
	}
	defer func() {
		for _, closer := range closers {
			_ = closer.Close()
		}
	}()

	var values []any
	for _, reader := range readers {
		decoder := json.NewDecoder(reader)
		for {
			var value any
			err := decoder.Decode(&value)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	}
	return values, nil
}

func writeValue(output io.Writer, value any, options options) error {
	if options.rawOutput {
		if text, ok := value.(string); ok {
			_, err := fmt.Fprintln(output, text)
			return err
		}
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if !options.compact {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

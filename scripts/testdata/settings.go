// Bridge the Python native verifier to the provider's actual settings codec.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	ch "github.com/colony-2/pulse-cloudhypervisor"
)

func convert(mode, path string) error {
	switch mode {
	case "read":
		c, err := ch.LoadConfig(path)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(c)
	case "write":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
		if err != nil {
			return err
		}
		c, err := ch.DecodeConfig(data)
		if err != nil {
			return err
		}
		return ch.SaveConfig(path, c)
	}
	return ch.ErrConfig
}

func main() {
	if len(os.Args) != 3 {
		os.Exit(2)
	}
	if err := convert(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

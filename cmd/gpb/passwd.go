package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"gpb/internal/config"
)

const minPasswordLength = 8

func runPasswd() error {
	cfg, err := config.Load(config.DataDir())
	if err != nil {
		return err
	}

	prompter := newPasswordPrompter(os.Stdin)

	password, err := prompter.read("New web UI password: ")
	if err != nil {
		return err
	}
	if len(password) < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}

	confirmation, err := prompter.read("Repeat password: ")
	if err != nil {
		return err
	}
	if password != confirmation {
		return errors.New("passwords do not match")
	}

	if err := cfg.SetPassword(password); err != nil {
		return err
	}

	fmt.Printf("password written to %s\n", config.Path(cfg.DataDir()))
	return nil
}

// passwordPrompter turns off echo on a terminal and otherwise reads plain lines, holding
// one buffered reader across prompts so piped input does not vanish into the first read.
type passwordPrompter struct {
	input  *os.File
	buffer *bufio.Reader
}

func newPasswordPrompter(input *os.File) *passwordPrompter {
	return &passwordPrompter{input: input}
}

func (p *passwordPrompter) read(prompt string) (string, error) {
	fmt.Print(prompt)

	if term.IsTerminal(int(p.input.Fd())) {
		entered, err := term.ReadPassword(int(p.input.Fd()))
		fmt.Println()
		return string(entered), err
	}

	if p.buffer == nil {
		p.buffer = bufio.NewReader(p.input)
	}

	line, err := p.buffer.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if errors.Is(err, io.EOF) && line != "" {
		err = nil
	}
	return line, err
}

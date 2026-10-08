package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/matthewalexandern/AI/internal/cognition"
)

// chat owns the interactive input for this command. Reading happens separately
// from the command loop so cancellation can interrupt a terminal awaiting input.
func chat(ctx context.Context, controller *cognition.Controller, session string, asJSON bool, in io.Reader, out, errs io.Writer) error {
	inputCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Closing stdin on cancellation also releases the blocked scanner goroutine.
	// A non-closable embedded reader may finish its Read later; it cannot hold up
	// the command or prevent the owned inference process from being reaped.
	if closer, ok := in.(io.Closer); ok {
		context.AfterFunc(inputCtx, func() { _ = closer.Close() })
	}
	type line struct {
		text string
		err  error
	}
	lines := make(chan line)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), (64<<10)+2)
		for scanner.Scan() {
			select {
			case lines <- line{text: scanner.Text()}:
			case <-inputCtx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- line{err: err}:
			case <-inputCtx.Done():
			}
		}
	}()
	fmt.Fprintln(out, "Mini Fabrics. Type /exit to leave. Session:", session)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprint(out, "> ")
		var next line
		select {
		case <-ctx.Done():
			return ctx.Err()
		case value, ok := <-lines:
			if err := ctx.Err(); err != nil {
				return err
			}
			if !ok {
				return nil
			}
			next = value
		}
		if next.err != nil {
			return next.err
		}
		input := strings.TrimSpace(next.text)
		if input == "/exit" {
			return nil
		}
		if input == "" {
			continue
		}
		result, err := turn(ctx, controller, session, input)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(errs, "turn:", err)
			continue
		}
		if err := printResult(out, result, asJSON); err != nil {
			return err
		}
	}
}

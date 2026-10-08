// Run a command on a desk, then stream another one's output as it runs.
//
//	GAIADESK_API_KEY=ak_… GAIADESK_TOKEN=gdagt_… go run ./examples/exec 123456789
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: exec <desk id>")
	}
	desk := os.Args[1]
	gd, err := gaiadesk.New(os.Getenv("GAIADESK_API_KEY"), gaiadesk.WithDeskToken(os.Getenv("GAIADESK_TOKEN")))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	r, err := gd.Exec(ctx, desk, gaiadesk.ExecRequest{Command: "uname -a"})
	switch {
	case errors.Is(err, gaiadesk.ErrUnreachable):
		log.Fatalf("desk %s is not reachable: %v", desk, err)
	case err != nil:
		log.Fatal(err)
	}
	fmt.Printf("exit %d: %s", r.Exit, r.Stdout)

	st, err := gd.ExecStream(ctx, desk, gaiadesk.ExecRequest{Argv: []string{"ls", "-la"}})
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	exit, err := st.Copy(os.Stdout, os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	if err := exit.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("exit", exit.ExitCode)
}

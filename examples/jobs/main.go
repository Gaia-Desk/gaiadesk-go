// Start a background job, follow its log, and wait for it.
//
//	GAIADESK_API_KEY=ak_… GAIADESK_TOKEN=gdagt_… go run ./examples/jobs 123456789
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: jobs <desk id>")
	}
	desk := os.Args[1]
	gd, err := gaiadesk.New(os.Getenv("GAIADESK_API_KEY"), gaiadesk.WithDeskToken(os.Getenv("GAIADESK_TOKEN")))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	job, err := gd.StartJob(ctx, desk, gaiadesk.JobRequest{Name: "nightly", Command: "./build.sh --release", Shell: gaiadesk.ShellBash, Env: map[string]string{"CI": "1"}, Priority: "low"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("started", job.Name)

	logs, err := gd.FollowJobLogs(ctx, desk, job.Name, nil)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := logs.Copy(os.Stdout, nil); err != nil {
		log.Fatal(err)
	}

	done, err := gd.WaitJob(ctx, desk, job.Name, gaiadesk.WaitForever)
	if err != nil {
		log.Fatal(err)
	}
	if done.Job.ExitCode != nil {
		fmt.Println("exit", *done.Job.ExitCode)
	}
}

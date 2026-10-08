// Copy a file to a desk and back.
//
//	GAIADESK_API_KEY=ak_… GAIADESK_TOKEN=gdagt_… go run ./examples/files 123456789 report.csv
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatal("usage: files <desk id> <local file>")
	}
	desk, local := os.Args[1], os.Args[2]
	gd, err := gaiadesk.New(os.Getenv("GAIADESK_API_KEY"), gaiadesk.WithDeskToken(os.Getenv("GAIADESK_TOKEN")),
		gaiadesk.WithE2E(gaiadesk.E2ERequire)) // never in the clear
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	up, err := gd.UploadFile(ctx, local, desk, "/tmp/")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("uploaded %d bytes to %s\n", up.Bytes, up.Destination)
	down, err := gd.DownloadFile(ctx, desk, up.Destination, local+".copy")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("downloaded %d bytes to %s\n", down.Bytes, down.Destination)
}

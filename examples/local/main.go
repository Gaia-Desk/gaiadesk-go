// On a desk: drive it through the GaiaDesk app's own API (no server, no
// internet), with the desk's local admin token found beside the socket.
//
//	go run ./examples/local
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	gd, err := gaiadesk.NewLocal()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	desks, err := gd.Desks(ctx) // this desk
	if err != nil {
		log.Fatal(err)
	}
	for _, d := range desks.Devices {
		st, err := gd.Stats(ctx, d.DeskID)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s (%s): %.0f%% CPU, %d MB free\n", d.DeskID, st.Hostname, st.CPUPercent, st.MemFreeMB)
	}
}

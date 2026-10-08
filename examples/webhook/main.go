// Receive GaiaDesk webhook deliveries, verifying each one.
//
//	GAIADESK_WEBHOOK_SECRET=whsec_… go run ./examples/webhook
package main

import (
	"io"
	"log"
	"net/http"
	"os"

	"github.com/Gaia-Desk/gaiadesk-go"
)

func main() {
	secret := os.Getenv("GAIADESK_WEBHOOK_SECRET")
	http.HandleFunc("/hooks/gaiadesk", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		ev, err := gaiadesk.ParseWebhook(secret, r.Header.Get("GaiaDesk-Signature"), body)
		if err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		switch ev.Type {
		case gaiadesk.EventDeskOffline:
			var d gaiadesk.DeskEventData
			if ev.DecodeData(&d) == nil {
				log.Printf("desk %s went offline: %s", d.Desk.DeskID, d.Desk.ReasonText)
			}
		case gaiadesk.EventJobFinished:
			var d gaiadesk.JobFinishedData
			if ev.DecodeData(&d) == nil {
				log.Printf("job %s on %s: %s", d.Job.Name, d.Desk.DeskID, d.Job.State)
			}
		}
		w.WriteHeader(http.StatusNoContent) // any 2xx acknowledges; de-duplicate by ev.ID
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}

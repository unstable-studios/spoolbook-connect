// Drives the demo: a custom material, a spool on tool 0 of the one printer,
// a link to spoolbook, then watches.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"time"
)

const base = "http://127.0.0.1:18080"
const mat = base + "/api/v1/modules/com.github.aux.prusa3d.materials"
const sb = base + "/api/v1/modules/com.github.aux.unstable-studios.spoolbook-connect"

var c *http.Client

func do(method, url string, body any) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func main() {
	jar, _ := cookiejar.New(nil)
	c = &http.Client{Jar: jar, Timeout: 20 * time.Second}
	for i := 0; ; i++ {
		s, b := do("POST", base+"/api/v1/auth/login", map[string]string{"username": "demo", "password": "demodemo1"})
		if s == 200 {
			break
		}
		if i > 30 {
			fmt.Println("login", s, string(b))
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
	s, b := do("POST", mat+"/custom-materials", map[string]any{"name": "Test PLA", "brand_name": "Prusament", "class": "FFF", "type": "PLA", "color_rgba": "#ff0000ff", "density": 1.24, "diameter_um": 1750})
	fmt.Println("custom material", s, string(b))
	var m struct{ UUID string `json:"uuid"` }
	_ = json.Unmarshal(b, &m)
	s, b = do("POST", mat+"/spools", map[string]any{"material_uuid": m.UUID, "remaining_weight_g": 1000, "full_weight_g": 1000, "quantity": 1})
	fmt.Println("spool", s, string(b))
	var added struct{ IDs []int64 `json:"ids"` }
	_ = json.Unmarshal(b, &added)
	s, b = do("GET", mat+"/printers", nil)
	fmt.Println("printers", s, string(b)[:min(len(b), 600)])
	var printers struct {
		Printers []struct{ ID string `json:"id"`; DeviceID string `json:"device_id"` } `json:"printers"`
	}
	_ = json.Unmarshal(b, &printers)
	dev := ""
	if len(printers.Printers) > 0 {
		dev = printers.Printers[0].ID + printers.Printers[0].DeviceID
	}
	s, b = do("GET", mat+"/printers/"+dev+"/tools", nil)
	fmt.Println("tools", s, string(b)[:min(len(b), 1200)])
	spool := added.IDs[0]
	for i := 0; i < 60; i++ {
		s, b = do("PUT", fmt.Sprintf("%s/printers/%s/tools/0", mat, dev), map[string]any{"spool_id": spool})
		if s == 200 {
			break
		}
		if i%10 == 0 {
			fmt.Println("assign", s, string(b))
		}
		time.Sleep(time.Second)
	}
	fmt.Println("assign", s, string(b))
	s, b = do("PUT", fmt.Sprintf("%s/links/%d", sb, spool), map[string]string{"vendor": "prusament", "filament": "pla-test", "spool": "s1"})
	fmt.Println("link", s, string(b))
	// a second spool on no tool: its amount left goes on its own, after the delay
	s, b = do("POST", mat+"/spools", map[string]any{"material_uuid": m.UUID, "remaining_weight_g": 500, "full_weight_g": 1000, "quantity": 1})
	fmt.Println("spool 2", s, string(b))
	_ = json.Unmarshal(b, &added)
	s, b = do("PUT", fmt.Sprintf("%s/links/%d", sb, added.IDs[0]), map[string]string{"vendor": "prusament", "filament": "pla-test", "spool": "s2"})
	fmt.Println("link 2", s, string(b))
	s, b = do("PATCH", fmt.Sprintf("%s/spools/%d", mat, added.IDs[0]), map[string]any{"remaining_weight_g": 432.1})
	fmt.Println("weigh spool 2", s, string(b)[:min(len(b), 200)])
	s, b = do("GET", sb+"/spools", nil)
	fmt.Println("our spools", s, string(b))
	s, b = do("GET", sb+"/page", nil)
	fmt.Println("page", s, len(b))
	for i := 0; i < 12; i++ { // three minutes: past the delay before an amount left goes
		time.Sleep(15 * time.Second)
		_, st := do("GET", sb+"/status", nil)
		fmt.Printf("t+%ds status=%.600s\n", (i+1)*15, st)
	}
}

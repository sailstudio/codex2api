package prismchannel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
)

var restrictedImageNetworks = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16"} {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

func publicImageIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, n := range restrictedImageNetworks {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
func imageURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, invalid("remote_image_requires_https")
	}
	return u, nil
}

// DNS is checked and the validated IP is dialed directly, including redirects,
// preventing a second resolution from rebinding to an internal service.
func imageClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, invalid("image_host")
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil || len(ips) == 0 {
			return nil, invalid("image_dns")
		}
		for _, ip := range ips {
			if !publicImageIP(ip.IP) {
				return nil, invalid("private_image_address")
			}
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return &http.Client{Transport: t, Timeout: 15 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return invalid("image_redirect_limit")
		}
		_, e := imageURL(r.URL.String())
		return e
	}}
}
func (c *Client) imageData(ctx context.Context, raw string) ([]byte, string, error) {
	var data []byte
	if strings.HasPrefix(raw, "data:") {
		comma := strings.IndexByte(raw, ',')
		if comma < 0 || !strings.HasPrefix(raw, "data:image/") || !strings.HasSuffix(raw[:comma], ";base64") {
			return nil, "", invalid("invalid_image_data_url")
		}
		if int64(base64.StdEncoding.DecodedLen(len(raw[comma+1:]))) > c.cfg.ImageBytes+2 {
			return nil, "", &Error{413, "image_too_large", ""}
		}
		var e error
		data, e = base64.StdEncoding.DecodeString(raw[comma+1:])
		if e != nil {
			return nil, "", invalid("invalid_image_base64")
		}
	} else {
		if !c.cfg.AllowRemoteImages {
			return nil, "", invalid("remote_images_disabled")
		}
		u, e := imageURL(raw)
		if e != nil {
			return nil, "", e
		}
		cl := imageClient()
		defer cl.CloseIdleConnections()
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if e != nil {
			return nil, "", invalid("image_request")
		}
		resp, e := cl.Do(req)
		if e != nil {
			return nil, "", invalid("image_fetch_failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, "", invalid("image_fetch_status")
		}
		data, e = io.ReadAll(io.LimitReader(resp.Body, c.cfg.ImageBytes+1))
		if e != nil {
			return nil, "", invalid("image_read_failed")
		}
	}
	if int64(len(data)) > c.cfg.ImageBytes {
		return nil, "", &Error{413, "image_too_large", ""}
	}
	ic, format, e := image.DecodeConfig(bytes.NewReader(data))
	if e != nil || ic.Width <= 0 || ic.Height <= 0 || int64(ic.Width)*int64(ic.Height) > 50_000_000 {
		return nil, "", invalid("invalid_or_oversize_image")
	}
	ext := map[string]string{"png": ".png", "jpeg": ".jpg", "gif": ".gif", "webp": ".webp"}[format]
	if ext == "" {
		return nil, "", invalid("unsupported_image_type")
	}
	return data, ext, nil
}
func (c *Client) preprocessImages(ctx context.Context, s *slot, input []Item) ([]Item, error) {
	out := append([]Item(nil), input...)
	inlineRemaining := 96 << 10
	for i, it := range out {
		out[i].Content = append([]Content(nil), it.Content...)
		for j, block := range it.Content {
			if block.Type != "input_image" {
				continue
			}
			data, ext, e := c.imageData(ctx, block.ImageURL)
			if e != nil {
				return nil, e
			}
			encoded := ""
			if c.cfg.ImageInline {
				encodedSize := base64.StdEncoding.EncodedLen(len(data))
				if encodedSize > 48<<10 || encodedSize > inlineRemaining {
					return nil, &Error{413, "image_inline_budget_exceeded", ""}
				}
				encoded = base64.StdEncoding.EncodeToString(data)
				inlineRemaining -= len(encoded)
			}
			name := "image_" + uuid.NewString() + ext
			fid := uuid.NewString()
			headers := http.Header{"X-Prism-File-Id": []string{fid}, "X-Prism-Project-Id": []string{s.project}, "X-Prism-File-Name": []string{url.QueryEscape(name)}, "X-Prism-File-Size": []string{strconv.Itoa(len(data))}, "X-Prism-Require-Project-Edit-Access": []string{"true"}}
			// The browser wire protocol sends raw image bytes, not a multipart wrapper.
			contentType := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[ext]
			raw, e := c.do(ctx, s.credential, http.MethodPost, "/api/project-files/upload", data, contentType, "", headers)
			if e != nil {
				return nil, e
			}
			var uploaded struct {
				ID         string `json:"id"`
				FileUUID   string `json:"fileUuid"`
				SedimentID string `json:"sedimentFileId"`
				URL        string `json:"url"`
				StorageURL string `json:"storage_url"`
				FileURL    string `json:"file_url"`
			}
			if json.Unmarshal(raw, &uploaded) != nil || uploaded.ID == "" && uploaded.FileUUID == "" {
				return nil, &Error{502, "upload_reference_missing", ""}
			}
			if uploaded.SedimentID != "" || uploaded.FileUUID != "" {
				c.metrics.UploadStorageReferences.Add(1)
			}
			if uploaded.URL != "" || uploaded.StorageURL != "" || uploaded.FileURL != "" {
				c.metrics.UploadURLs.Add(1)
			}
			c.metrics.Uploads.Add(1)
			out[i].Content[j] = Content{Type: "input_file", Filename: name, ProjectPath: "/prism-uploads/" + name}
			if c.cfg.ImageInline {
				// Headless uploads can be absent from the editor's materialized file tree.
				// Only generated filenames and validated base64 enter the restoration command.
				out[i].Content[j] = Content{Type: "input_text", Text: fmt.Sprintf("Image attachment. Restore these bytes in the workspace with the shell tool, then use view_image to inspect the resulting image before answering. Do not guess from the base64 or repeat it in the answer.\nmkdir -p prism-uploads && printf '%%s' '%s' | base64 -d > prism-uploads/%s\nThen view_image prism-uploads/%s", encoded, name, name)}
			}
		}
	}
	return out, nil
}

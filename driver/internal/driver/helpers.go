package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
)

var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (d *Driver) owns(labels map[string]string) bool {
	owner, ok := labels[labelOwner]
	if !ok {
		owner = DefaultOwner
	}
	return owner == sanitizeLabel(d.cfg.Owner)
}

// image picks the Blaxel image. Blaxel images must embed Blaxel's sandbox-api;
// arbitrary OCI images (such as OpenShell's community image) do not.
func (d *Driver) image(r *record, sb *pb.DriverSandbox) string {
	img := sb.GetSpec().GetTemplate().GetImage()
	if img == "" {
		return d.cfg.DefaultImage
	}
	// Blaxel images must embed Blaxel's sandbox-api; arbitrary OCI images
	// (such as OpenShell's community base image) do not.
	if strings.HasPrefix(img, "blaxel/") || strings.HasPrefix(img, "sandbox/") {
		return img
	}
	d.event(r, "Warning", "ImageSubstituted",
		fmt.Sprintf("image %q is not a Blaxel sandbox image; using %s", img, d.cfg.DefaultImage))
	return d.cfg.DefaultImage
}
func (d *Driver) memoryMiB(sb *pb.DriverSandbox) (int, error) {
	q := sb.GetSpec().GetTemplate().GetResources().GetMemoryLimit()
	if q == "" {
		return d.cfg.MemoryMiB, nil
	}
	units := []struct {
		suffix string
		mib    float64
	}{{"Gi", 1024}, {"Mi", 1}, {"G", 1e9 / (1 << 20)}, {"M", 1e6 / (1 << 20)}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(q, u.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory limit %q", q)
			}
			return int(v * u.mib), nil
		}
	}
	v, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory limit %q", q)
	}
	return int(v >> 20), nil
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// blaxelName derives a stable, DNS-safe Blaxel sandbox name.
func blaxelName(name, id string) string {
	slug := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 20 {
		slug = strings.Trim(slug[:20], "-")
	}
	sum := sha256.Sum256([]byte(id))
	return "os-" + slug + "-" + hex.EncodeToString(sum[:4])
}

var nonLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeLabel(s string) string {
	s = nonLabel.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

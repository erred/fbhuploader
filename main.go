package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"golang.org/x/oauth2/google"
	firebasehosting "google.golang.org/api/firebasehosting/v1beta1"
)

func main() {
	flag.Parse()

	ctx := context.Background()
	err := run(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	fbConfFile := "firebase.json"

	fbConf, err := readConfig(fbConfFile)
	if err != nil {
		return err
	}

	httpClient, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/firebase")
	if err != nil {
		return fmt.Errorf("create http client: %w", err)
	}

	client, err := firebasehosting.NewService(ctx)
	if err != nil {
		return fmt.Errorf("create firebase client: %w", err)
	}

	version, err := createVersion(ctx, client, fbConf)
	if err != nil {
		return err
	}

	pathToHash, hashToGzip, err := readFiles(ctx, fbConf)
	if err != nil {
		return err
	}

	toUpload, uploadURL, err := getRequiredUploads(ctx, client, version, pathToHash)
	if err != nil {
		return err
	}

	err = uploadFiles(ctx, client, httpClient, version, toUpload, uploadURL, hashToGzip)
	if err != nil {
		return err
	}

	err = release(ctx, client, "sites/"+fbConf.Hosting.Site, version)
	if err != nil {
		return err
	}

	return nil
}

func readConfig(fbConfFile string) (*FirebaseJSON, error) {
	b, err := os.ReadFile(fbConfFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fbConfFile, err)
	}
	var fbConf FirebaseJSON
	err = json.Unmarshal(b, &fbConf)
	if err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", fbConfFile, err)
	}
	return &fbConf, nil
}

func createVersion(ctx context.Context, client *firebasehosting.Service, fbConf *FirebaseJSON) (string, error) {
	servingConf := &firebasehosting.ServingConfig{
		CleanUrls: fbConf.Hosting.CleanURLs,
	}
	if fbConf.Hosting.TrailingSlash {
		servingConf.TrailingSlashBehavior = "ADD"
	}
	for _, header := range fbConf.Hosting.Headers {
		hdrs := make(map[string]string)
		for _, hdr := range header.Headers {
			hdrs[hdr.Key] = hdr.Value
		}
		servingConf.Headers = append(servingConf.Headers, &firebasehosting.Header{
			Glob:    header.Source,
			Headers: hdrs,
		})
	}
	for _, redirect := range fbConf.Hosting.Redirects {
		servingConf.Redirects = append(servingConf.Redirects, &firebasehosting.Redirect{
			Glob:       redirect.Source,
			Location:   redirect.Destination,
			StatusCode: int64(redirect.Type),
		})
	}

	siteID := "sites/" + fbConf.Hosting.Site
	version, err := client.Sites.Versions.Create(siteID, &firebasehosting.Version{
		Config: servingConf,
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create new version for %s: %w", siteID, err)
	}
	return version.Name, nil
}

func readFiles(ctx context.Context, fbConf *FirebaseJSON) (map[string]string, map[string]io.Reader, error) {
	pathToHash := make(map[string]string)
	hashToGzip := make(map[string]io.Reader)
	dirFS := os.DirFS(fbConf.Hosting.Public)
	err := fs.WalkDir(dirFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// TODO: check not in ignores
		f, err := dirFS.Open(p)
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		defer f.Close()

		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, err = io.Copy(gw, f)
		if err != nil {
			return fmt.Errorf("read from %s: %w", p, err)
		}
		err = gw.Close()
		if err != nil {
			return fmt.Errorf("flush gzip writer for %s: %w", p, err)
		}
		sum := sha256.Sum256(buf.Bytes())
		hash := hex.EncodeToString(sum[:])
		pathToHash["/"+p] = hash
		hashToGzip[hash] = &buf

		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk %s: %w", fbConf.Hosting.Public, err)
	}
	return pathToHash, hashToGzip, nil
}

func getRequiredUploads(ctx context.Context, client *firebasehosting.Service, version string, pathToHash map[string]string) ([]string, string, error) {
	populateResponse, err := client.Sites.Versions.PopulateFiles(version, &firebasehosting.PopulateVersionFilesRequest{
		Files: pathToHash,
	}).Context(ctx).Do()
	if err != nil {
		return nil, "", fmt.Errorf("get required uploads for %s: %w", version, err)
	}
	return populateResponse.UploadRequiredHashes, populateResponse.UploadUrl, nil
}

func uploadFiles(ctx context.Context, client *firebasehosting.Service, httpClient *http.Client, version string, toUpload []string, uploadURL string, hashToGzip map[string]io.Reader) error {
	for _, uploadHash := range toUpload {
		endpoint := uploadURL + "/" + uploadHash
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, hashToGzip[uploadHash])
		if err != nil {
			return fmt.Errorf("create request for %s: %w", uploadHash, err)
		}
		req.Header.Set("content-type", "application/octet-stream")
		res, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("upload for %s: %w", uploadHash, err)
		}
		if res.StatusCode != 200 {
			return fmt.Errorf("unexpected response for upload %s: %v", uploadHash, res.Status)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
	}

	patchResponse, err := client.Sites.Versions.Patch(version, &firebasehosting.Version{
		Status: "FINALIZED",
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("finalize %s: %w", version, err)
	}
	if patchResponse.Status != "FINALIZED" {
		return fmt.Errorf("unexpected finalization status: %v", patchResponse.Status)
	}
	return nil
}

func release(ctx context.Context, client *firebasehosting.Service, site, version string) error {
	_, err := client.Sites.Releases.Create(site, &firebasehosting.Release{}).VersionName(version).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("release %s: %w", version, err)
	}
	return nil
}

type FirebaseJSON struct {
	Hosting struct {
		Site          string   `json:"site"`
		Public        string   `json:"public"`
		Ignore        []string `json:"ignore"`
		CleanURLs     bool     `json:"cleanUrls"`
		TrailingSlash bool     `json:"trailingSlash"`
		Headers       []struct {
			Source  string `json:"source"`
			Headers []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"headers"`
		Redirects []struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Type        int    `json:"type"`
		} `json:"redirects"`
	} `json:"hosting"`
}

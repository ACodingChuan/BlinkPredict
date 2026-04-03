package marketconfirm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type MetadataDocument struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Image       string `json:"image"`
	ImageURL    string `json:"image_url"`
}

func metadataURLFromCID(cid string) string {
	cid = strings.TrimSpace(cid)
	if cid == "" {
		return ""
	}
	return "ipfs://" + cid
}

func fetchMetadata(ctx context.Context, cid string) (MetadataDocument, string, error) {
	url := metadataURLFromCID(cid)
	if url == "" {
		return MetadataDocument{}, "", fmt.Errorf("metadata cid is empty")
	}
	httpURL := ipfsGatewayURL(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL, nil)
	if err != nil {
		return MetadataDocument{}, url, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return MetadataDocument{}, url, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MetadataDocument{}, url, fmt.Errorf("metadata http status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return MetadataDocument{}, url, err
	}
	var doc MetadataDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return MetadataDocument{}, url, err
	}
	if doc.ImageURL == "" {
		doc.ImageURL = doc.Image
	}
	if strings.HasPrefix(doc.ImageURL, "ipfs://") {
		doc.ImageURL = ipfsGatewayURL(doc.ImageURL)
	}
	return doc, url, nil
}

func ipfsGatewayURL(uri string) string {
	trimmed := strings.TrimSpace(uri)
	if strings.HasPrefix(trimmed, "ipfs://") {
		return "https://ipfs.io/ipfs/" + strings.TrimPrefix(trimmed, "ipfs://")
	}
	return trimmed
}

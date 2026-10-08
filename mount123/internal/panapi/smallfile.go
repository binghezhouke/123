package panapi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxSmallFileBytes   = 1 << 20
	maxZIPPasswordBytes = 4096
)

// ReadSmallFile downloads a file only when its complete content fits maxBytes.
// It is intended for small sidecar files; the returned bytes are never logged.
func (c *Client) ReadSmallFile(ctx context.Context, id, maxBytes int64) ([]byte, error) {
	if id <= 0 {
		return nil, errors.New("panapi: file ID must be positive")
	}
	if maxBytes <= 0 || maxBytes > maxSmallFileBytes {
		return nil, errors.New("panapi: small-file read limit must be between 1 byte and 1 MiB")
	}
	link, err := c.DownloadURL(ctx, id)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, errors.New("panapi: could not create download request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("panapi: small-file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("panapi: small-file download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, errors.New("panapi: small file exceeds read limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("panapi: could not read small-file response")
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("panapi: small file exceeds read limit")
	}
	return data, nil
}

// SaveZIPPassword stores password bytes beside archive as <archive name>.pwd.
// An existing sidecar is reused when it matches and replaced when it differs.
func (c *Client) SaveZIPPassword(ctx context.Context, archive File, password []byte) (File, error) {
	if archive.ID <= 0 || archive.IsDir || archive.ParentID < 0 || archive.Name == "" || path.Base(archive.Name) != archive.Name || strings.ContainsAny(archive.Name, `\\/:*?"<>|`) || !strings.EqualFold(path.Ext(archive.Name), ".zip") {
		return File{}, errors.New("panapi: invalid archive metadata")
	}
	if len(password) == 0 || len(password) > maxZIPPasswordBytes || !utf8.Valid(password) {
		return File{}, errors.New("panapi: ZIP password must be valid UTF-8 between 1 byte and 4096 bytes")
	}
	name := archive.Name + ".pwd"
	if len(name) > 255 {
		return File{}, errors.New("panapi: ZIP password sidecar filename exceeds 255 bytes")
	}
	files, err := c.List(ctx, archive.ParentID)
	if err != nil {
		return File{}, err
	}
	var existingSidecar *File
	for i := range files {
		existing := files[i]
		if existing.Name != name {
			continue
		}
		if existingSidecar != nil {
			return File{}, errors.New("panapi: multiple sibling .pwd files exist")
		}
		if existing.IsDir {
			return File{}, errors.New("panapi: sibling .pwd path is a directory")
		}
		existingSidecar = &existing
	}
	if existingSidecar != nil && existingSidecar.Size <= maxZIPPasswordBytes {
		old, err := c.ReadSmallFile(ctx, existingSidecar.ID, maxZIPPasswordBytes)
		if err != nil {
			if ctx.Err() != nil {
				return File{}, ctx.Err()
			}
			return File{}, errors.New("panapi: existing sibling .pwd file cannot be safely compared")
		}
		if bytes.Equal(old, password) {
			return *existingSidecar, nil
		}
	}

	md5sum := md5.Sum(password)
	// Always keep the canonical name, including when another unlock command
	// creates the same sidecar between List and Create. Auto-renaming would
	// produce a file the mount never consults.
	duplicate := 2
	var created struct {
		FileID    int64    `json:"fileID"`
		Reuse     bool     `json:"reuse"`
		Preupload string   `json:"preuploadID"`
		SliceSize int64    `json:"sliceSize"`
		Servers   []string `json:"servers"`
	}
	createPayload := struct {
		ParentID   int64  `json:"parentFileID"`
		Filename   string `json:"filename"`
		ETag       string `json:"etag"`
		Size       int    `json:"size"`
		Duplicate  int    `json:"duplicate"`
		ContainDir bool   `json:"containDir"`
	}{archive.ParentID, name, hex.EncodeToString(md5sum[:]), len(password), duplicate, false}
	body, err := json.Marshal(createPayload)
	if err != nil {
		return File{}, errors.New("panapi: could not encode upload request")
	}
	if err := c.requestJSON(ctx, http.MethodPost, "/upload/v2/file/create", nil, body, &created); err != nil {
		if ctx.Err() != nil {
			return File{}, ctx.Err()
		}
		return File{}, errors.New("panapi: could not create ZIP password sidecar")
	}
	if created.Reuse {
		if created.FileID <= 0 {
			return File{}, errors.New("panapi: upload reuse response missing file ID")
		}
		return File{ID: created.FileID, ParentID: archive.ParentID, Name: name, Size: int64(len(password)), Version: hex.EncodeToString(md5sum[:]) + ":" + strconv.Itoa(len(password))}, nil
	}
	if created.Preupload == "" || created.SliceSize <= 0 || len(created.Servers) == 0 {
		return File{}, errors.New("panapi: upload response missing required fields")
	}
	for offset, sliceNo := int64(0), int64(1); offset < int64(len(password)); sliceNo++ {
		end := offset + created.SliceSize
		if end > int64(len(password)) {
			end = int64(len(password))
		}
		part := password[offset:end]
		server := created.Servers[(sliceNo-1)%int64(len(created.Servers))]
		if err := c.uploadSmallFileSlice(ctx, server, created.Preupload, sliceNo, part); err != nil {
			return File{}, err
		}
		offset = end
	}
	var completed struct {
		Completed bool  `json:"completed"`
		FileID    int64 `json:"fileID"`
	}
	completePayload, _ := json.Marshal(struct {
		Preupload string `json:"preuploadID"`
	}{created.Preupload})
	if err := c.requestJSON(ctx, http.MethodPost, "/upload/v2/file/upload_complete", nil, completePayload, &completed); err != nil {
		if ctx.Err() != nil {
			return File{}, ctx.Err()
		}
		return File{}, errors.New("panapi: could not complete ZIP password sidecar upload")
	}
	if !completed.Completed || completed.FileID <= 0 {
		return File{}, errors.New("panapi: upload completion was not confirmed")
	}
	return File{ID: completed.FileID, ParentID: archive.ParentID, Name: name, Size: int64(len(password)), Version: hex.EncodeToString(md5sum[:]) + ":" + strconv.Itoa(len(password))}, nil
}

func (c *Client) uploadSmallFileSlice(ctx context.Context, server, preupload string, sliceNo int64, content []byte) error {
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	base, err := url.Parse(server)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return errors.New("panapi: invalid upload server")
	}
	endpoint := strings.TrimRight(server, "/") + "/upload/v2/file/slice"
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("preuploadID", preupload); err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	if err := writer.WriteField("sliceNo", strconv.FormatInt(sliceNo, 10)); err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	md5sum := md5.Sum(content)
	if err := writer.WriteField("sliceMD5", hex.EncodeToString(md5sum[:])); err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	part, err := writer.CreateFormFile("slice", "slice")
	if err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	if _, err := part.Write(content); err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	if err := writer.Close(); err != nil {
		return errors.New("panapi: could not encode upload slice")
	}
	if err := c.waitRateLimit(ctx); err != nil {
		return err
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &form)
	if err != nil {
		return errors.New("panapi: could not create upload slice request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Platform", "open_platform")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("panapi: upload slice request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("panapi: could not read upload slice response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("panapi: upload slice returned HTTP %d", resp.StatusCode)
	}
	var result apiEnvelope
	if err := json.Unmarshal(data, &result); err != nil || result.Code != 0 {
		return errors.New("panapi: upload slice was not accepted")
	}
	return nil
}

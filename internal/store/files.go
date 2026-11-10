package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Attachments live in memory only, like messages: a file is dropped once every
// message carrying it has been acknowledged (or expired), and on restart.
const (
	MaxFile       = 25 << 20  // per file
	MaxFilesTotal = 200 << 20 // everything held at once, uploads in progress included
	MaxInline     = 1 << 20   // inline (base64 in a tool call) uploads
	MaxAttach     = 10        // attachments per message
	uploadTTL     = 15 * time.Minute
	unattachedTTL = time.Hour // uploaded but never sent
)

var (
	ErrTooBig      = fmt.Errorf("%w: files are capped at 25 MB", ErrBadRequest)
	ErrStorageFull = errors.New("server file storage is full")
)

// Attachment is what a message carries. DownloadURL is a path; the HTTP layer
// prefixes the server's public origin.
type Attachment struct {
	ID          string `json:"file_id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

type file struct {
	Attachment
	hid, owner  string
	data        []byte // nil until uploaded
	uploadHash  string // pending upload token
	uploadUntil time.Time
	created     time.Time
	refs        int // undelivered messages carrying it
	attached    bool
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || len(name) > 200 {
		return "", fmt.Errorf("%w: name is required (1-200 chars)", ErrBadRequest)
	}
	return name, nil
}

func cleanType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" || len(ct) > 100 || strings.ContainsAny(ct, "\r\n") {
		return "application/octet-stream"
	}
	return ct
}

// reserve claims size bytes of the total. Caller holds mu.
func (s *Store) reserve(size int64) error {
	if size <= 0 || size > MaxFile {
		if size <= 0 {
			return fmt.Errorf("%w: size must be positive", ErrBadRequest)
		}
		return ErrTooBig
	}
	if s.fileBytes+size > MaxFilesTotal {
		return ErrStorageFull
	}
	s.fileBytes += size
	return nil
}

func (s *Store) newFile(a *Agent, name, ctype string, size int64) (*file, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	if err := s.reserve(size); err != nil {
		return nil, err
	}
	id := "fcpf_" + randHex(24) // unguessable: it is also the download capability
	f := &file{Attachment: Attachment{ID: id, Name: name, ContentType: cleanType(ctype), Size: size, DownloadURL: "/v1/files/" + id},
		hid: a.HouseholdID, owner: a.ID, created: time.Now()}
	s.files[id] = f
	return f, nil
}

// NewUpload declares a file and returns a one-time token for uploading its bytes.
func (s *Store) NewUpload(a *Agent, name, ctype string, size int64) (id, token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcFiles()
	f, err := s.newFile(a, name, ctype, size)
	if err != nil {
		return "", "", err
	}
	token = "fcpu_" + randHex(24)
	f.uploadHash, f.uploadUntil = Hash(token), time.Now().Add(uploadTTL)
	return f.ID, token, nil
}

func (s *Store) pending(token string) *file {
	h := Hash(token)
	for _, f := range s.files {
		if f.data == nil && f.uploadHash == h && time.Now().Before(f.uploadUntil) {
			return f
		}
	}
	return nil
}

// UploadSize is the declared size for a pending upload token (to cap the body).
func (s *Store) UploadSize(token string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.pending(token); f != nil {
		return f.Size, nil
	}
	return 0, ErrGone
}

// PutUpload stores the bytes for a pending upload. The token works once.
func (s *Store) PutUpload(token string, data []byte) (Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.pending(token)
	if f == nil {
		return Attachment{}, ErrGone
	}
	if int64(len(data)) != f.Size {
		return Attachment{}, fmt.Errorf("%w: got %d bytes, declared %d", ErrBadRequest, len(data), f.Size)
	}
	f.data, f.uploadHash, f.created = data, "", time.Now()
	return f.Attachment, nil
}

// AddFile stores a file in one step (authenticated REST upload, or inline base64).
func (s *Store) AddFile(a *Agent, name, ctype string, data []byte) (Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcFiles()
	f, err := s.newFile(a, name, ctype, int64(len(data)))
	if err != nil {
		return Attachment{}, err
	}
	f.data = data
	return f.Attachment, nil
}

// File returns a stored file by id (the download capability).
func (s *Store) File(id string) (Attachment, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	f, ok := s.files[id]
	if !ok || f.data == nil {
		return Attachment{}, nil, ErrNotFound
	}
	return f.Attachment, f.data, nil
}

// attach validates the sender's file ids and returns their metadata. Caller holds mu.
func (s *Store) attach(from *Agent, ids []string) ([]Attachment, error) {
	if len(ids) > MaxAttach {
		return nil, fmt.Errorf("%w: at most %d attachments per message", ErrBadRequest, MaxAttach)
	}
	var out []Attachment
	for _, id := range ids {
		f, ok := s.files[id]
		if !ok || f.hid != from.HouseholdID || f.owner != from.ID || f.data == nil {
			return nil, fmt.Errorf("%w: unknown or not-yet-uploaded file_id %q", ErrBadRequest, id)
		}
		out = append(out, f.Attachment)
	}
	return out, nil
}

// retain counts n more undelivered copies of each attachment. Caller holds mu.
func (s *Store) retain(atts []Attachment, n int) {
	for _, a := range atts {
		if f := s.files[a.ID]; f != nil {
			f.refs += n
			f.attached = true
		}
	}
}

// release drops one undelivered copy of each attachment in msgs, freeing
// files nothing refers to any more. Caller holds mu.
func (s *Store) release(msgs []Message) {
	for _, m := range msgs {
		for _, a := range m.Attachments {
			if f := s.files[a.ID]; f != nil {
				if f.refs--; f.refs <= 0 && f.attached {
					s.dropFile(f)
				}
			}
		}
	}
}

func (s *Store) dropFile(f *file) {
	delete(s.files, f.ID)
	s.fileBytes -= f.Size
}

// gcFiles drops abandoned uploads and files uploaded but never sent. Caller holds mu.
func (s *Store) gcFiles() {
	now := time.Now()
	for _, f := range s.files {
		switch {
		case f.data == nil && now.After(f.uploadUntil):
			s.dropFile(f)
		case !f.attached && now.Sub(f.created) > unattachedTTL:
			s.dropFile(f)
		}
	}
}

// WithBase returns copies of msgs whose attachment download URLs are absolute.
func WithBase(msgs []Message, base string) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		if len(m.Attachments) > 0 {
			atts := make([]Attachment, len(m.Attachments))
			for j, a := range m.Attachments {
				a.DownloadURL = base + a.DownloadURL
				atts[j] = a
			}
			m.Attachments = atts
		}
		out[i] = m
	}
	return out
}

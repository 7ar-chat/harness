package classification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service coordinates detection, metadata writing and verification (service.py).
type Service struct {
	settings  Settings
	adapters  map[Format]adapter
	providers map[string]Provider
	// Now and NewUUID are the clock and id source (overridable for golden tests).
	Now     func() time.Time
	NewUUID func() uuid.UUID
	// slots bounds concurrent classifications (each may hold ~3× the document in memory; C-08).
	slots chan struct{}
}

// DefaultMaxConcurrent is the default Settings.MaxConcurrent.
const DefaultMaxConcurrent = 4

// New builds a service; block mode may run without provider settings.
func New(s Settings) (*Service, error) {
	if len(s.Profile.Providers) == 0 {
		return nil, errf(ErrConfiguration, "profile must configure at least one provider")
	}
	if s.MaxInputBytes <= 0 || s.MaxOutputBytes <= 0 {
		return nil, errf(ErrConfiguration, "classification size limits must be positive")
	}
	svc := &Service{
		settings: s,
		adapters: map[Format]adapter{
			PDF:  pdfAdapter{},
			DOCX: ooxmlAdapter{format: DOCX},
			XLSX: ooxmlAdapter{format: XLSX},
			PPTX: ooxmlAdapter{format: PPTX},
		},
		providers: map[string]Provider{"mip": MIP{}, "gvdata": GVData{}},
		Now:       time.Now,
		NewUUID:   uuid.New,
	}
	n := s.MaxConcurrent
	if n <= 0 {
		n = DefaultMaxConcurrent
	}
	svc.slots = make(chan struct{}, n)
	if s.Mode != "block" {
		for _, p := range s.Profile.Providers {
			if _, ok := svc.providers[p]; !ok {
				return nil, errf(ErrConfiguration, "configured provider is not registered: %s", p)
			}
		}
	}
	return svc, nil
}

// Settings returns the validated settings.
func (s *Service) Settings() Settings { return s.settings }

func (s *Service) context(actor string) Context {
	if strings.TrimSpace(actor) == "" {
		actor = s.settings.Actor
	}
	return Context{
		Timestamp:      s.Now().UTC(),
		DocumentID:     s.NewUUID(),
		ActionID:       s.NewUUID(),
		LineID:         s.NewUUID(),
		Actor:          strings.TrimSpace(actor),
		ProfileName:    s.settings.Profile.Name,
		ProfileVersion: s.settings.Profile.Version,
	}
}

func (s *Service) providerProperties(ctx Context) ([]string, map[string]string, error) {
	var keys []string
	combined := map[string]string{}
	for _, name := range s.settings.Profile.Providers {
		p, ok := s.providers[name]
		if !ok {
			return nil, nil, errf(ErrConfiguration, "provider is not registered: %s", name)
		}
		ks, vals, err := p.Properties(ctx, s.settings.Profile.ProviderSettings[name])
		if err != nil {
			return nil, nil, err
		}
		for _, k := range ks {
			v := vals[k]
			if strings.TrimSpace(k) == "" {
				return nil, nil, errf(ErrProvider, "provider %s returned invalid properties", name)
			}
			nk := strings.TrimPrefix(k, "/")
			if _, dup := combined[nk]; dup {
				return nil, nil, errf(ErrProvider, "duplicate provider metadata key: %s", nk)
			}
			keys = append(keys, nk)
			combined[nk] = v
		}
	}
	return keys, combined, nil
}

// Classify labels one complete document or returns a typed, fail-closed error
// (always wrapping ErrClassification).
func (s *Service) Classify(ctx context.Context, filename, contentType string, content []byte, actor string) (res *Result, err error) {
	start := time.Now()
	if int64(len(content)) > s.settings.MaxInputBytes {
		return nil, errf(ErrSizeExceeded, "classification input exceeds configured limit")
	}
	format, err := Detect(filename, contentType, content)
	if err != nil {
		return nil, err
	}
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: waiting for a classification slot: %w", ErrClassification, ctx.Err())
		}
	}
	if format == "" {
		return nil, errf(ErrUnsupportedFormat, "document format is not supported")
	}
	if s.settings.Mode == "block" {
		return nil, errf(ErrUnsupportedFormat, "classification policy blocks supported documents")
	}
	ad := s.adapters[format]
	lctx := s.context(actor)
	log := slog.With("document_id", lctx.DocumentID.String(), "format", string(format), "input_bytes", len(content),
		"profile", s.settings.Profile.Name, "profile_version", s.settings.Profile.Version,
		"providers", s.settings.Profile.Providers, "actor", lctx.Actor)
	defer func() {
		if p := recover(); p != nil {
			res, err = nil, errf(ErrProvider, "classification failed")
		}
		if err != nil {
			category := "unexpected"
			for _, e := range []error{ErrSizeExceeded, ErrInvalidDocument, ErrProvider, ErrVerificationFailed, ErrConfiguration, ErrFormatMismatch, ErrUnsupportedFormat} {
				if errors.Is(err, e) {
					category = e.Error()
					break
				}
			}
			log.Warn("file_classification_failed", "error_category", category, "duration_ms", time.Since(start).Milliseconds())
			return
		}
		log.Info("file_classification_completed", "output_bytes", len(res.Content), "duration_ms", time.Since(start).Milliseconds())
	}()
	source, err := ad.inspect(content)
	if err != nil {
		return nil, err
	}
	// A document already labelled C4 / C3-Amundi-Only (or whose label cannot be
	// read) is never relabelled for delivery (C-04).
	if labels, lerr := ConfidentialityLabels(content); lerr != nil {
		return nil, errf(ErrInvalidDocument, "the document's existing label cannot be read")
	} else if l := mostRestrictive(labels); ConfidentialityDisallowed(l) {
		return nil, errf(ErrInvalidDocument, "the document already carries a disallowed confidentiality label")
	}
	keys, props, err := s.providerProperties(lctx)
	if err != nil {
		return nil, err
	}
	out, err := ad.writeMetadata(source, keys, props, s.ownedKey)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > s.settings.MaxOutputBytes {
		return nil, errf(ErrSizeExceeded, "classification output exceeds configured limit")
	}
	if err := ad.verify(out, keys, props, source); err != nil {
		return nil, err
	}
	for _, name := range s.settings.Profile.Providers {
		if err := s.providers[name].Verify(props, lctx, s.settings.Profile.ProviderSettings[name]); err != nil {
			return nil, err
		}
	}
	return &Result{Content: out, Format: format, Context: lctx, Providers: s.settings.Profile.Providers, Properties: props}, nil
}

// ownedKey reports an existing document property written by one of the
// configured providers (an earlier label): it is replaced, never kept next to
// the new one (C-04, C-07).
func (s *Service) ownedKey(key string) bool {
	key = strings.TrimPrefix(key, "/")
	for _, name := range s.settings.Profile.Providers {
		switch name {
		case "mip":
			if strings.HasPrefix(key, "MSIP_Label_") {
				return true
			}
		case "gvdata":
			if gvDataKey.MatchString(key) {
				return true
			}
			for _, k := range gvWrittenKeys {
				if key == k {
					return true
				}
			}
		}
	}
	return false
}

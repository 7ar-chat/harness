package product

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/store"
)

// Profile. The display name defaults to the first and last name from the
// identity provider; the user may set their own and upload an avatar.
//
//	GET    /api/me                       → {user, projects}
//	PATCH  /api/me/profile {displayName} → user   ("" resets to the default name)
//	PUT    /api/me/avatar  <image bytes> → user   (png, jpeg, webp or gif, at most 512 KB)
//	DELETE /api/me/avatar                → user
//	GET    /api/users/{login}/avatar     → the image

const maxAvatarBytes = 512 << 10

// userView is a user as the API shows them.
type userView struct {
	Login           string `json:"login"`
	Name            string `json:"name"`
	DisplayName     string `json:"displayName"`
	DefaultName     string `json:"defaultName"`
	Email           string `json:"email,omitempty"`
	AvatarURL       string `json:"avatarUrl"`
	HasCustomAvatar bool   `json:"hasCustomAvatar"`
}

func viewOf(u store.User) userView {
	def := u.Name
	if def == "" {
		def = u.Login
	}
	return userView{Login: u.Login, Name: u.Label(), DisplayName: u.DisplayName, DefaultName: def, Email: u.Email,
		AvatarURL: u.Avatar(), HasCustomAvatar: u.AvatarVersion != ""}
}

func (s *server) routeProfile() {
	s.Route("PATCH /me/profile", s.patchProfile)
	s.Route("PUT /me/avatar", s.putAvatar)
	s.Route("DELETE /me/avatar", s.deleteAvatar)
	s.Route("GET /users/{login}/avatar", s.getAvatar)
}

func (s *server) currentUser(r *http.Request, c Caller) (*store.User, error) {
	u, err := s.team.GetUser(r.Context(), c.Actor)
	if errors.Is(err, store.ErrNotFound) {
		return &store.User{Login: c.Actor, Name: c.Actor}, nil
	}
	return u, err
}

func cleanName(s string) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 80 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return s, true
}

func (s *server) patchProfile(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		DisplayName *string `json:"displayName"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.DisplayName != nil {
		name, ok := cleanName(*body.DisplayName)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "The display name must be at most 80 characters, without control characters.")
			return
		}
		if err := s.team.SetDisplayName(r.Context(), c.Actor, name); err != nil {
			s.profileFail(w, err)
			return
		}
	}
	s.writeUser(w, r, c)
}

func (s *server) profileFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Your account is not registered yet; log in again.")
		return
	}
	fail(w, err)
}

func (s *server) writeUser(w http.ResponseWriter, r *http.Request, c Caller) {
	u, err := s.currentUser(r, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewOf(*u))
}

var avatarTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

func (s *server) putAvatar(w http.ResponseWriter, r *http.Request, c Caller) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+1)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) > maxAvatarBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "The avatar must be at most 512 KB.")
		return
	}
	// The type is sniffed from the bytes, never taken from the request (no SVG: it can carry script).
	mime := http.DetectContentType(data)
	if len(data) == 0 || !avatarTypes[mime] {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "The avatar must be a PNG, JPEG, WebP or GIF image.")
		return
	}
	if _, err := s.team.SetAvatar(r.Context(), c.Actor, mime, data); err != nil {
		s.profileFail(w, err)
		return
	}
	s.writeUser(w, r, c)
}

func (s *server) deleteAvatar(w http.ResponseWriter, r *http.Request, c Caller) {
	if _, err := s.team.SetAvatar(r.Context(), c.Actor, "", nil); err != nil {
		s.profileFail(w, err)
		return
	}
	s.writeUser(w, r, c)
}

func (s *server) getAvatar(w http.ResponseWriter, r *http.Request, c Caller) {
	mime, data, err := s.team.GetAvatar(r.Context(), r.PathValue("login"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No avatar.")
			return
		}
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("Cache-Control", "private, max-age=86400") // the URL carries the version
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = w.Write(data)
}

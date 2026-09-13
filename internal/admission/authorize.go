package admission

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrNotFound        = errors.New("deployment not found")
	ErrUnavailable     = errors.New("admission dependency unavailable")
)

func ExtractBearer(value string) (string, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || parts[1] == "" {
		return "", ErrUnauthenticated
	}
	return parts[1], nil
}

func ParseDeploymentPath(path string) (string, error) {
	path = strings.SplitN(path, "?", 2)[0]
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "deployments" || parts[3] != "chat" || parts[4] != "completions" {
		return "", ErrNotFound
	}
	if parts[2] == "" || len(parts[2]) > 64 || strings.ContainsAny(parts[2], " \\") {
		return "", ErrNotFound
	}
	parsed, err := uuid.Parse(parts[2])
	if err != nil || parsed.Version() != 7 {
		return "", ErrNotFound
	}
	return parts[2], nil
}

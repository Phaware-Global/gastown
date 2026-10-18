package sweep

import (
	"regexp"
	"strconv"
	"time"
)

// Shape is one allowed value shape for a Fields entry. Everything that fails
// its key's shape is human-authored as far as the runner is concerned and is
// moved to External, where it is fenced.
type Shape int

const (
	ShapeInt Shape = iota + 1
	ShapeTime
	ShapeSHA
	ShapeJiraAccountID
	ShapeGitHubLogin
	ShapeBeadID
	ShapeEnum
)

const maxFieldLen = 64

var (
	shaRe           = regexp.MustCompile(`^[0-9a-f]{40}$`)
	jiraAccountRe   = regexp.MustCompile(`^(\d+:[0-9a-f-]{36}|[0-9a-f]{24})$`)
	githubLoginRe   = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
	beadIDRe        = regexp.MustCompile(`^[a-z0-9]{1,8}-[a-z0-9.-]{1,40}$`)
	enumRe          = regexp.MustCompile(`^[a-z_]{1,32}$`)
	allowedShapeLen = map[Shape]int{ShapeGitHubLogin: 39}
)

// ValidShape reports whether v matches shape (and its length cap).
func ValidShape(shape Shape, v string) bool {
	if v == "" || len(v) > maxFieldLen {
		return false
	}
	if n, ok := allowedShapeLen[shape]; ok && len(v) > n {
		return false
	}
	switch shape {
	case ShapeInt:
		_, err := strconv.ParseInt(v, 10, 64)
		return err == nil
	case ShapeTime:
		_, err := time.Parse(time.RFC3339, v)
		return err == nil
	case ShapeSHA:
		return shaRe.MatchString(v)
	case ShapeJiraAccountID:
		return jiraAccountRe.MatchString(v)
	case ShapeGitHubLogin:
		return githubLoginRe.MatchString(v)
	case ShapeBeadID:
		return beadIDRe.MatchString(v)
	case ShapeEnum:
		return enumRe.MatchString(v)
	}
	return false
}

// enforceSchema moves every Fields entry that is undeclared or fails its
// shape into External. It never drops data.
func enforceSchema(schema map[string]Shape, f *Finding) {
	for k, v := range f.Fields {
		shape, declared := schema[k]
		if declared && ValidShape(shape, v) {
			continue
		}
		if f.External == nil {
			f.External = map[string]string{}
		}
		f.External[k] = v
		delete(f.Fields, k)
	}
}

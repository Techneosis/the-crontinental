package main

// Tags are a job's own words for its notify hooks: key=value pairs set with
// --tag when it is booked (or edited, or on one `notify`), handed to every hook
// as CRONTINENTAL_TAG_<KEY> and as "tags" in the JSON. Crontinental never reads
// them; what `topic=ops` means is the hooks' business. Stored as one JSON
// object, so the ledger holds any number without a column each.

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
)

// A key becomes part of an environment variable's name, so it is held to what
// a shell can spell. Stored lowercase; the variable is uppercase.
var tagKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

const tagEnvPrefix = "CRONTINENTAL_TAG_"

func checkTagKey(k string) string {
	k = strings.ToLower(k)
	if !tagKey.MatchString(k) {
		die("%q is not a tag name: letters, digits and _, starting with a letter", k)
	}
	return k
}

// parseTags reads --tag key=value flags, the last one winning for a key.
func parseTags(flags []string) map[string]string {
	tags := map[string]string{}
	for _, f := range flags {
		k, v, ok := strings.Cut(f, "=")
		if !ok || v == "" {
			die("--tag %q needs a value: --tag key=value (--no-tag key, on edit, removes one)", f)
		}
		tags[checkTagKey(k)] = v
	}
	return tags
}

// tagsValue is what the ledger stores: NULL for no tags.
func tagsValue(tags map[string]string) any {
	if len(tags) == 0 {
		return nil
	}
	b, _ := json.Marshal(tags) // map keys marshal sorted
	return string(b)
}

func jobTags(j *Job) map[string]string {
	tags := map[string]string{}
	if j != nil && j.Tags.Valid {
		if err := json.Unmarshal([]byte(j.Tags.String), &tags); err != nil {
			die("%s: its tags in the ledger are not readable: %v", j.Name, err)
		}
	}
	return tags
}

func sortedTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func describeTags(tags map[string]string) string {
	var parts []string
	for _, k := range sortedTagKeys(tags) {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}

// hookEnviron is the environment a hook starts from: the caller's, less any
// tag variables it inherited, so a hook sees exactly this event's tags.
func hookEnviron() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, tagEnvPrefix) {
			env = append(env, e)
		}
	}
	return env
}

func tagEnv(tags map[string]string) []string {
	var env []string
	for _, k := range sortedTagKeys(tags) {
		env = append(env, tagEnvPrefix+strings.ToUpper(k)+"="+tags[k])
	}
	return env
}

// rawTags is the ledger's tags column as JSON for `list --json`: an object, {}
// when there are none.
func rawTags(v any) json.RawMessage {
	s := ""
	switch t := v.(type) {
	case string:
		s = t
	case []byte:
		s = string(t)
	}
	if s == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

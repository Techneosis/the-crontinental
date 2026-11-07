package main

// Tags are a job's own words for its notify hooks and its command: key=value
// pairs set with --tag when it is booked (or edited, or on one `notify`),
// handed to every hook as CRONTINENTAL_TAG_<KEY> and as "tags" in the JSON, and
// to every run's command as the same variables (each run gets the tags as they
// stood when it started). CRONTINENTAL_TAGS carries the whole set as one JSON
// object. Crontinental never reads them; what `topic=ops` means is the hooks'
// business. Stored as one JSON object, so the ledger holds any number without
// a column each. They are labels, not secrets: plain in the ledger, the
// message book, `show`, and every run's and hook's environment.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// A key becomes part of an environment variable's name, so it is held to what
// a shell can spell. Stored lowercase; the variable is uppercase.
var tagKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

const tagEnvPrefix = "CRONTINENTAL_TAG_"

// Every tag reaches every run's environment, where one variable can be no
// longer than the kernel allows (128KiB on Linux) and the whole environment
// shares one limit, so a job's tags are kept small: a value up to 1KiB, and
// the set up to 8KiB as JSON.
const (
	maxTagValue = 1024
	maxTagSet   = 8 * 1024
)

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
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			die("--tag %s: the value must be text (UTF-8, no NUL)", k)
		}
		if len(v) > maxTagValue {
			die("--tag %s: the value is %d bytes; a tag holds up to %d", k, len(v), maxTagValue)
		}
		tags[checkTagKey(k)] = v
	}
	return tags
}

// checkTagSet refuses a set of tags too big for every run to carry.
func checkTagSet(tags map[string]string) {
	b, _ := json.Marshal(tags)
	if len(b) > maxTagSet {
		die("the tags come to %d bytes; a job's tags hold up to %d in all", len(b), maxTagSet)
	}
}

// tagsValue is what the ledger stores: NULL for no tags.
func tagsValue(tags map[string]string) any {
	if len(tags) == 0 {
		return nil
	}
	b, _ := json.Marshal(tags) // map keys marshal sorted
	return string(b)
}

// jobTags is a job's tags. A tags column that can't be read is said on stderr
// and taken as none, never fatal: exec reads it mid-run, and `edit --tag` has to
// be able to write a good one over it.
func jobTags(j *Job) map[string]string {
	tags := map[string]string{}
	if j != nil && j.Tags.Valid {
		if err := json.Unmarshal([]byte(j.Tags.String), &tags); err != nil {
			fmt.Fprintf(os.Stderr, "crontinental: %s: its tags in the ledger are not readable (%v); going on without them\n", j.Name, err)
			return map[string]string{}
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

// tagsEnvVar holds the whole set as one JSON object, for a reader that wants
// them all without scanning the environment.
const tagsEnvVar = "CRONTINENTAL_TAGS"

// isTagVar says whether an environment entry is one of the tag variables.
func isTagVar(e string) bool {
	return strings.HasPrefix(e, tagEnvPrefix) || strings.HasPrefix(e, tagsEnvVar+"=")
}

// hookEnviron is the environment a hook starts from: the caller's, less any
// tag variables it inherited, so a hook sees exactly this event's tags.
func hookEnviron() []string {
	var env []string
	for _, e := range os.Environ() {
		if !isTagVar(e) {
			env = append(env, e)
		}
	}
	return env
}

// tagEnv is the tag variables for a set of tags: one CRONTINENTAL_TAG_<KEY>
// each, and CRONTINENTAL_TAGS with all of them ({} for none). A hook gets its
// event's; a run's command gets its job's.
// Only the CLI checks what goes into the ledger, so a tag that would make a bad
// variable (a key no shell can spell, a value with a NUL or that isn't text, or
// one too big) is left out of both the variables and the set, and said on
// stderr, which is the run's or the hook's log.
func tagEnv(tags map[string]string) []string {
	good := map[string]string{}
	for k, v := range tags {
		if tagKey.MatchString(k) && utf8.ValidString(v) && !strings.ContainsRune(v, 0) && len(v) <= maxTagValue {
			good[k] = v
		} else {
			fmt.Fprintf(os.Stderr, "crontinental: tag %q can't be passed on as a variable; left out\n", k)
		}
	}
	b, _ := json.Marshal(good) // map keys marshal sorted
	if len(b) > maxTagSet {
		fmt.Fprintf(os.Stderr, "crontinental: the tags come to %d bytes, over %d; none are passed on\n", len(b), maxTagSet)
		good, b = map[string]string{}, []byte("{}")
	}
	var env []string
	for _, k := range sortedTagKeys(good) {
		env = append(env, tagEnvPrefix+strings.ToUpper(k)+"="+good[k])
	}
	return append(env, tagsEnvVar+"="+string(b))
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

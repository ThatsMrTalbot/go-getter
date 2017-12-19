package getter

import (
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HttpGetter is a Getter implementation that will download from an HTTP
// endpoint.
//
// For file downloads, HTTP is used directly.
//
// The protocol for downloading a directory from an HTTP endpoing is as follows:
//
// An HTTP GET request is made to the URL with the additional GET parameter
// "terraform-get=1". This lets you handle that scenario specially if you
// wish. The response must be a 2xx.
//
// First, a header is looked for "X-Terraform-Get" which should contain
// a source URL to download.
//
// If the header is not present, then a meta tag is searched for named
// "terraform-get" and the content should be a source URL.
//
// The source URL, whether from the header or meta tag, must be a fully
// formed URL. The shorthand syntax of "github.com/foo/bar" or relative
// paths are not allowed.
type HttpGetter struct {
	// Netrc, if true, will lookup and use auth information found
	// in the user's netrc file if available.
	Netrc bool

	// Client is the http.Client to use for Get requests.
	// This defaults to a cleanhttp.DefaultClient if left unset.
	Client *http.Client
}

func (g *HttpGetter) ClientMode(u *url.URL) (ClientMode, error) {
	if strings.HasSuffix(u.Path, "/") {
		return ClientModeDir, nil
	}
	return ClientModeFile, nil
}

func (g *HttpGetter) Get(dst string, u *url.URL) error {
	// Copy the URL so we can modify it
	var newU url.URL = *u
	u = &newU

	if g.Netrc {
		// Add auth from netrc if we can
		if err := addAuthFromNetrc(u); err != nil {
			return err
		}
	}

	if g.Client == nil {
		g.Client = httpClient
	}

	// Add terraform-get to the parameter.
	q := u.Query()
	q.Add("terraform-get", "1")
	u.RawQuery = q.Encode()

	// Get the URL
	resp, err := g.Client.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bad response code: %d", resp.StatusCode)
	}

	// Extract the source URL
	var source string
	if v := resp.Header.Get("X-Terraform-Get"); v != "" {
		source = v
	} else {
		source, err = g.parseMeta(resp.Body)
		if err != nil {
			return err
		}
	}
	if source == "" {
		return fmt.Errorf("no source URL was returned")
	}

	// If there is a subdir component, then we download the root separately
	// into a temporary directory, then copy over the proper subdir.
	source, subDir := SourceDirSubdir(source)
	if subDir == "" {
		return Get(dst, source)
	}

	// We have a subdir, time to jump some hoops
	return g.getSubdir(dst, source, subDir)
}

func (g *HttpGetter) GetFile(dst string, u *url.URL) error {
	if g.Netrc {
		// Add auth from netrc if we can
		if err := addAuthFromNetrc(u); err != nil {
			return err
		}
	}

	if g.Client == nil {
		g.Client = httpClient
	}

	// check to see whether user has specified a range of bytes to download.
	// if user has, but the range is invalid, fall back to downloading the
	// whole file
	byteRange, rangeErr := getByteRange(u)
	if rangeErr != nil {
		// log that we are downloading whole file even though user
		// requested a byte range.
		log.Printf("%s; going to disregard range request and download entire file",
			rangeErr)
	}

	// Create new Request here because if it is a range request, we need to set
	// Range headers on the request object.
	req, err := http.NewRequest("HEAD", u.String(), nil)

	isPartialDownload := false
	if rangeErr == nil && byteRange != nil {
		// We first make a HEAD request so we can check if the server supports
		// range queries. If the server/URL doesn't support HEAD requests,
		// we just fall back to GET.

		if err != nil {
			return err
		}

		resp, err := g.Client.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// If the HEAD request succeeded, then attempt to set the range
			// query if we can.
			if resp.Header.Get("Accept-Ranges") == "bytes" {
				req.Header.Set("Range", fmt.Sprintf("bytes=%s-%s",
					byteRange[0], byteRange[1]))
				isPartialDownload = true
			}
		} else {
			log.Printf("HEAD request for Range failed; falling back to full file download: %s",
				err)
		}
	}

	// Set the request to GET now, and redo the query to download
	req.Method = "GET"

	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// if we did a ranged request we'll get a 206 instead.
		if !((resp.StatusCode == 206) && (isPartialDownload == true)) {
			return fmt.Errorf("bad response code: %d", resp.StatusCode)
		}
	}

	// Create all the parent directories
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// getSubdir downloads the source into the destination, but with
// the proper subdir.
func (g *HttpGetter) getSubdir(dst, source, subDir string) error {
	// Create a temporary directory to store the full source
	td, err := ioutil.TempDir("", "tf")
	if err != nil {
		return err
	}
	defer os.RemoveAll(td)

	// We have to create a subdirectory that doesn't exist for the file
	// getter to work.
	td = filepath.Join(td, "data")

	// Download that into the given directory
	if err := Get(td, source); err != nil {
		return err
	}

	// Process any globbing
	sourcePath, err := SubdirGlob(td, subDir)
	if err != nil {
		return err
	}

	// Make sure the subdir path actually exists
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf(
			"Error downloading %s: %s", source, err)
	}

	// Copy the subdirectory into our actual destination.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}

	// Make the final destination
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	return copyDir(dst, sourcePath, false)
}

// parseMeta looks for the first meta tag in the given reader that
// will give us the source URL.
func (g *HttpGetter) parseMeta(r io.Reader) (string, error) {
	d := xml.NewDecoder(r)
	d.CharsetReader = charsetReader
	d.Strict = false
	var err error
	var t xml.Token
	for {
		t, err = d.Token()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return "", err
		}
		if e, ok := t.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			return "", nil
		}
		if e, ok := t.(xml.EndElement); ok && strings.EqualFold(e.Name.Local, "head") {
			return "", nil
		}
		e, ok := t.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}
		if attrValue(e.Attr, "name") != "terraform-get" {
			continue
		}
		if f := attrValue(e.Attr, "content"); f != "" {
			return f, nil
		}
	}
}

// getByteRange is a helper function to parse out the byte range for ranged
// requests.
//
// input values:
// u must be non-nil and unmodified.
// example of u:
// "http://my/file.iso?ranged_request_bytes=5555-66666"
// "http://my/file.iso?ranged_request_bytes=5555-"
// note that the format of the raw url string can be either
// bytes=startByte- OR bytes=startByte-endByte
//
// return values:
// (non-nil, non-nil) - should not occur
// (nil, nil) - returned when no byte range was provided in the first place
// (nil, non-nil) - error occured when parsing or validating the byte range
// (non-nil, nil) - byte range was successfully parsed, and the range is
// returned as an array of strings in the following format:
// example non-nil return val "vals":
// []string{5555, 66666}      // first url example above
// []string{5555}             // second url example above
func getByteRange(u *url.URL) ([]string, error) {
	q := u.Query()
	byteRange := q.Get("ranged_request_bytes")
	if byteRange == "" {
		return nil, nil
	}

	vals := strings.Split(byteRange, "-")

	// validate byte range values given
	if len(vals) != 2 {
		return nil, fmt.Errorf("%s; length of parsed byte range is not 2, %v",
			invalidRangeMsg, vals)
	}
	start, err := strconv.ParseInt(vals[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s; could not convert start byte string to an integer",
			invalidRangeMsg)
	}
	// If you leave endBytes blank, read to end of file.
	if vals[1] != "" {
		// otherwise make sure "finish" value is a number
		finish, err := strconv.ParseInt(vals[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s; could not convert finish byte string to an integer",
				invalidRangeMsg)
		}
		// need finish to be bigger than start val
		if finish <= start {
			return nil, fmt.Errorf("%s; finish byte must be bigger than start byte",
				invalidRangeMsg)
		}
	}

	return vals, nil
}

// attrValue returns the attribute value for the case-insensitive key
// `name', or the empty string if nothing is found.
func attrValue(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

// charsetReader returns a reader for the given charset. Currently
// it only supports UTF-8 and ASCII. Otherwise, it returns a meaningful
// error which is printed by go get, so the user can find why the package
// wasn't downloaded if the encoding is not supported. Note that, in
// order to reduce potential errors, ASCII is treated as UTF-8 (i.e. characters
// greater than 0x7f are not rejected).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "ascii":
		return input, nil
	default:
		return nil, fmt.Errorf("can't decode XML document using charset %q", charset)
	}
}

const invalidRangeMsg = fmt.Errorf("Invalid byte range provided")

package gateway

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"net/url"
	gopath "path"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/go-path/resolver"
	coreiface "github.com/ipfs/interface-go-ipfs-core"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	mc "github.com/multiformats/go-multicodec"
	prometheus "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

var log = logging.Logger("core/server")

const (
	ipfsPathPrefix        = "/ipfs/"
	ipnsPathPrefix        = "/ipns/"
	immutableCacheControl = "public, max-age=29030400, immutable"
)

var (
	onlyASCII = regexp.MustCompile("[[:^ascii:]]")
	noModtime = time.Unix(0, 0) // disables Last-Modified header if passed as modtime

	ErrGatewayTimeout = errors.New(http.StatusText(http.StatusGatewayTimeout))
	ErrBadGateway     = errors.New(http.StatusText(http.StatusBadGateway))
)

// HTML-based redirect for errors which can be recovered from, but we want
// to provide hint to people that they should fix things on their end.
var redirectTemplate = template.Must(template.New("redirect").Parse(`<!DOCTYPE html>
<html>
	<head>
		<meta charset="utf-8">
		<meta http-equiv="refresh" content="10;url={{.RedirectURL}}" />
		<link rel="canonical" href="{{.RedirectURL}}" />
	</head>
	<body>
		<pre>{{.ErrorMsg}}</pre><pre>(if a redirect does not happen in 10 seconds, use "{{.SuggestedPath}}" instead)</pre>
	</body>
</html>`))

type redirectTemplateData struct {
	RedirectURL   string
	SuggestedPath string
	ErrorMsg      string
}

// handler is a HTTP handler that serves IPFS objects (accessible by default at /ipfs/<path>)
// (it serves requests like GET /ipfs/QmVRzPKPzNtSrEzBFm2UZfxmPAgnaLke4DMcerbsGGSaFe/link)
type handler struct {
	config Config
	api    API

	// generic metrics
	firstContentBlockGetMetric *prometheus.HistogramVec
	unixfsGetMetric            *prometheus.SummaryVec // deprecated, use firstContentBlockGetMetric

	// response type metrics
	getMetric                    *prometheus.HistogramVec
	unixfsFileGetMetric          *prometheus.HistogramVec
	unixfsDirIndexGetMetric      *prometheus.HistogramVec
	unixfsGenDirListingGetMetric *prometheus.HistogramVec
	carStreamGetMetric           *prometheus.HistogramVec
	rawBlockGetMetric            *prometheus.HistogramVec
	tarStreamGetMetric           *prometheus.HistogramVec
	jsoncborDocumentGetMetric    *prometheus.HistogramVec
	ipnsRecordGetMetric          *prometheus.HistogramVec
}

// StatusResponseWriter enables us to override HTTP Status Code passed to
// WriteHeader function inside of http.ServeContent.  Decision is based on
// presence of HTTP Headers such as Location.
type statusResponseWriter struct {
	http.ResponseWriter
}

// Custom type for collecting error details to be handled by `webRequestError`
type requestError struct {
	StatusCode int
	Err        error
}

func (r *requestError) Error() string {
	return r.Err.Error()
}

func newRequestError(err error, statusCode int) *requestError {
	return &requestError{
		Err:        err,
		StatusCode: statusCode,
	}
}

func (sw *statusResponseWriter) WriteHeader(code int) {
	// Check if we need to adjust Status Code to account for scheduled redirect
	// This enables us to return payload along with HTTP 301
	// for subdomain redirect in web browsers while also returning body for cli
	// tools which do not follow redirects by default (curl, wget).
	redirect := sw.ResponseWriter.Header().Get("Location")
	if redirect != "" && code == http.StatusOK {
		code = http.StatusMovedPermanently
		log.Debugw("subdomain redirect", "location", redirect, "status", code)
	}
	sw.ResponseWriter.WriteHeader(code)
}

// ServeContent replies to the request using the content in the provided ReadSeeker
// and returns the status code written and any error encountered during a write.
// It wraps http.ServeContent which takes care of If-None-Match+Etag,
// Content-Length and range requests.
func ServeContent(w http.ResponseWriter, req *http.Request, name string, modtime time.Time, content io.ReadSeeker) (int, bool, error) {
	ew := &errRecordingResponseWriter{ResponseWriter: w}
	http.ServeContent(ew, req, name, modtime, content)

	// When we calculate some metrics we want a flag that lets us to ignore
	// errors and 304 Not Modified, and only care when requested data
	// was sent in full.
	dataSent := ew.code/100 == 2 && ew.err == nil

	return ew.code, dataSent, ew.err
}

// errRecordingResponseWriter wraps a ResponseWriter to record the status code and any write error.
type errRecordingResponseWriter struct {
	http.ResponseWriter
	code int
	err  error
}

func (w *errRecordingResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *errRecordingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// ReadFrom exposes errRecordingResponseWriter's underlying ResponseWriter to io.Copy
// to allow optimized methods to be taken advantage of.
func (w *errRecordingResponseWriter) ReadFrom(r io.Reader) (n int64, err error) {
	n, err = io.Copy(w.ResponseWriter, r)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

func newSummaryMetric(name string, help string) *prometheus.SummaryVec {
	summaryMetric := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "ipfs",
			Subsystem: "http",
			Name:      name,
			Help:      help,
		},
		[]string{"gateway"},
	)
	if err := prometheus.Register(summaryMetric); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			summaryMetric = are.ExistingCollector.(*prometheus.SummaryVec)
		} else {
			log.Errorf("failed to register ipfs_http_%s: %v", name, err)
		}
	}
	return summaryMetric
}

func newHistogramMetric(name string, help string) *prometheus.HistogramVec {
	// We can add buckets as a parameter in the future, but for now using static defaults
	// suggested in https://github.com/ipfs/kubo/issues/8441
	defaultBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}
	histogramMetric := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ipfs",
			Subsystem: "http",
			Name:      name,
			Help:      help,
			Buckets:   defaultBuckets,
		},
		[]string{"gateway"},
	)
	if err := prometheus.Register(histogramMetric); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			histogramMetric = are.ExistingCollector.(*prometheus.HistogramVec)
		} else {
			log.Errorf("failed to register ipfs_http_%s: %v", name, err)
		}
	}
	return histogramMetric
}

// NewHandler returns an http.Handler that can act as a gateway to IPFS content
// offlineApi is a version of the API that should not make network requests for missing data
func NewHandler(c Config, api API) http.Handler {
	return newHandler(c, api)
}

func newHandler(c Config, api API) *handler {
	i := &handler{
		config: c,
		api:    api,
		// Improved Metrics
		// ----------------------------
		// Time till the first content block (bar in /ipfs/cid/foo/bar)
		// (format-agnostic, across all response types)
		firstContentBlockGetMetric: newHistogramMetric(
			"gw_first_content_block_get_latency_seconds",
			"The time till the first content block is received on GET from the gateway.",
		),

		// Response-type specific metrics
		// ----------------------------
		// Generic: time it takes to execute a successful gateway request (all request types)
		getMetric: newHistogramMetric(
			"gw_get_duration_seconds",
			"The time to GET a successful response to a request (all content types).",
		),
		// UnixFS: time it takes to return a file
		unixfsFileGetMetric: newHistogramMetric(
			"gw_unixfs_file_get_duration_seconds",
			"The time to serve an entire UnixFS file from the gateway.",
		),
		// UnixFS: time it takes to find and serve an index.html file on behalf of a directory.
		unixfsDirIndexGetMetric: newHistogramMetric(
			"gw_unixfs_dir_indexhtml_get_duration_seconds",
			"The time to serve an index.html file on behalf of a directory from the gateway. This is a subset of gw_unixfs_file_get_duration_seconds.",
		),
		// UnixFS: time it takes to generate static HTML with directory listing
		unixfsGenDirListingGetMetric: newHistogramMetric(
			"gw_unixfs_gen_dir_listing_get_duration_seconds",
			"The time to serve a generated UnixFS HTML directory listing from the gateway.",
		),
		// CAR: time it takes to return requested CAR stream
		carStreamGetMetric: newHistogramMetric(
			"gw_car_stream_get_duration_seconds",
			"The time to GET an entire CAR stream from the gateway.",
		),
		// Block: time it takes to return requested Block
		rawBlockGetMetric: newHistogramMetric(
			"gw_raw_block_get_duration_seconds",
			"The time to GET an entire raw Block from the gateway.",
		),
		// TAR: time it takes to return requested TAR stream
		tarStreamGetMetric: newHistogramMetric(
			"gw_tar_stream_get_duration_seconds",
			"The time to GET an entire TAR stream from the gateway.",
		),
		// JSON/CBOR: time it takes to return requested DAG-JSON/-CBOR document
		jsoncborDocumentGetMetric: newHistogramMetric(
			"gw_jsoncbor_get_duration_seconds",
			"The time to GET an entire DAG-JSON/CBOR block from the gateway.",
		),
		// IPNS Record: time it takes to return IPNS record
		ipnsRecordGetMetric: newHistogramMetric(
			"gw_ipns_record_get_duration_seconds",
			"The time to GET an entire IPNS Record from the gateway.",
		),

		// Legacy Metrics
		// ----------------------------
		unixfsGetMetric: newSummaryMetric( // TODO: remove?
			// (deprecated, use firstContentBlockGetMetric instead)
			"unixfs_get_latency_seconds",
			"DEPRECATED: does not do what you think, use gw_first_content_block_get_latency_seconds instead.",
		),
	}
	return i
}

func (i *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// the hour is a hard fallback, we don't expect it to happen, but just in case
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	r = r.WithContext(ctx)

	defer func() {
		if r := recover(); r != nil {
			log.Error("A panic occurred in the gateway handler!")
			log.Error(r)
			debug.PrintStack()
		}
	}()

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		i.getOrHeadHandler(w, r)
		return
	case http.MethodOptions:
		i.optionsHandler(w, r)
		return
	}

	w.Header().Add("Allow", http.MethodGet)
	w.Header().Add("Allow", http.MethodHead)
	w.Header().Add("Allow", http.MethodOptions)

	errmsg := "Method " + r.Method + " not allowed: read only access"
	http.Error(w, errmsg, http.StatusMethodNotAllowed)
}

func (i *handler) optionsHandler(w http.ResponseWriter, r *http.Request) {
	/*
		OPTIONS is a noop request that is used by the browsers to check
		if server accepts cross-site XMLHttpRequest (indicated by the presence of CORS headers)
		https://developer.mozilla.org/en-US/docs/Web/HTTP/Access_control_CORS#Preflighted_requests
	*/
	i.addUserHeaders(w) // return all custom headers (including CORS ones, if set)
}

func (i *handler) getOrHeadHandler(w http.ResponseWriter, r *http.Request) {
	begin := time.Now()

	logger := log.With("from", r.RequestURI)
	logger.Debug("http request received")

	if err := handleUnsupportedHeaders(r); err != nil {
		webRequestError(w, err)
		return
	}

	if requestHandled := handleProtocolHandlerRedirect(w, r, logger); requestHandled {
		return
	}

	if err := handleServiceWorkerRegistration(r); err != nil {
		webRequestError(w, err)
		return
	}

	contentPath := ipath.New(r.URL.Path)
	ctx := context.WithValue(r.Context(), ContentPathKey, contentPath)
	r = r.WithContext(ctx)

	if requestHandled := i.handleOnlyIfCached(w, r, contentPath, logger); requestHandled {
		return
	}

	if requestHandled := handleSuperfluousNamespace(w, r, contentPath); requestHandled {
		return
	}

	// Detect when explicit Accept header or ?format parameter are present
	responseFormat, formatParams, err := customResponseFormat(r)
	if err != nil {
		webError(w, fmt.Errorf("error while processing the Accept header: %w", err), http.StatusBadRequest)
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("ResponseFormat", responseFormat))

	resolvedPath, contentPath, ok := i.handlePathResolution(w, r, responseFormat, contentPath, logger)
	if !ok {
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("ResolvedPath", resolvedPath.String()))

	// Detect when If-None-Match HTTP header allows returning HTTP 304 Not Modified
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		pathCid := resolvedPath.Cid()
		// need to check against both File and Dir Etag variants
		// because this inexpensive check happens before we do any I/O
		cidEtag := getEtag(r, pathCid)
		dirEtag := getDirListingEtag(pathCid)
		if etagMatch(inm, cidEtag, dirEtag) {
			// Finish early if client already has a matching Etag
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	if err := i.handleGettingFirstBlock(r, begin, contentPath, resolvedPath); err != nil {
		webRequestError(w, err)
		return
	}

	if err := i.setCommonHeaders(w, r, contentPath); err != nil {
		webRequestError(w, err)
		return
	}

	var success bool

	// Support custom response formats passed via ?format or Accept HTTP header
	switch responseFormat {
	case "", "application/json", "application/cbor":
		switch mc.Code(resolvedPath.Cid().Prefix().Codec) {
		case mc.Json, mc.DagJson, mc.Cbor, mc.DagCbor:
			logger.Debugw("serving codec", "path", contentPath)
			success = i.serveCodec(r.Context(), w, r, resolvedPath, contentPath, begin, responseFormat)
		default:
			logger.Debugw("serving unixfs", "path", contentPath)
			success = i.serveUnixFS(r.Context(), w, r, resolvedPath, contentPath, begin, logger)
		}
	case "application/vnd.ipld.raw":
		logger.Debugw("serving raw block", "path", contentPath)
		success = i.serveRawBlock(r.Context(), w, r, resolvedPath, contentPath, begin)
	case "application/vnd.ipld.car":
		logger.Debugw("serving car stream", "path", contentPath)
		carVersion := formatParams["version"]
		success = i.serveCAR(r.Context(), w, r, resolvedPath, contentPath, carVersion, begin)
	case "application/x-tar":
		logger.Debugw("serving tar file", "path", contentPath)
		success = i.serveTAR(r.Context(), w, r, resolvedPath, contentPath, begin, logger)
	case "application/vnd.ipld.dag-json", "application/vnd.ipld.dag-cbor":
		logger.Debugw("serving codec", "path", contentPath)
		success = i.serveCodec(r.Context(), w, r, resolvedPath, contentPath, begin, responseFormat)
	case "application/vnd.ipfs.ipns-record":
		logger.Debugw("serving ipns record", "path", contentPath)
		success = i.serveIpnsRecord(r.Context(), w, r, resolvedPath, contentPath, begin, logger)
	default: // catch-all for unsuported application/vnd.*
		err := fmt.Errorf("unsupported format %q", responseFormat)
		webError(w, err, http.StatusBadRequest)
		return
	}

	if success {
		i.getMetric.WithLabelValues(contentPath.Namespace()).Observe(time.Since(begin).Seconds())
	}
}

func (i *handler) addUserHeaders(w http.ResponseWriter) {
	for k, v := range i.config.Headers {
		w.Header()[k] = v
	}
}

func addCacheControlHeaders(w http.ResponseWriter, r *http.Request, contentPath ipath.Path, fileCid cid.Cid) (modtime time.Time) {
	// Set Etag to based on CID (override whatever was set before)
	w.Header().Set("Etag", getEtag(r, fileCid))

	// Set Cache-Control and Last-Modified based on contentPath properties
	if contentPath.Mutable() {
		// mutable namespaces such as /ipns/ can't be cached forever

		/* For now we set Last-Modified to Now() to leverage caching heuristics built into modern browsers:
		 * https://github.com/ipfs/kubo/pull/8074#pullrequestreview-645196768
		 * but we should not set it to fake values and use Cache-Control based on TTL instead */
		modtime = time.Now()

		// TODO: set Cache-Control based on TTL of IPNS/DNSLink: https://github.com/ipfs/kubo/issues/1818#issuecomment-1015849462
		// TODO: set Last-Modified based on /ipns/ publishing timestamp?
	} else {
		// immutable! CACHE ALL THE THINGS, FOREVER! wolololol
		w.Header().Set("Cache-Control", immutableCacheControl)

		// Set modtime to 'zero time' to disable Last-Modified header (superseded by Cache-Control)
		modtime = noModtime

		// TODO: set Last-Modified? - TBD - /ipfs/ modification metadata is present in unixfs 1.5 https://github.com/ipfs/kubo/issues/6920?
	}

	return modtime
}

// Set Content-Disposition if filename URL query param is present, return preferred filename
func addContentDispositionHeader(w http.ResponseWriter, r *http.Request, contentPath ipath.Path) string {
	/* This logic enables:
	 * - creation of HTML links that trigger "Save As.." dialog instead of being rendered by the browser
	 * - overriding the filename used when saving subresource assets on HTML page
	 * - providing a default filename for HTTP clients when downloading direct /ipfs/CID without any subpath
	 */

	// URL param ?filename=cat.jpg triggers Content-Disposition: [..] filename
	// which impacts default name used in "Save As.." dialog
	name := getFilename(contentPath)
	urlFilename := r.URL.Query().Get("filename")
	if urlFilename != "" {
		disposition := "inline"
		// URL param ?download=true triggers Content-Disposition: [..] attachment
		// which skips rendering and forces "Save As.." dialog in browsers
		if r.URL.Query().Get("download") == "true" {
			disposition = "attachment"
		}
		setContentDispositionHeader(w, urlFilename, disposition)
		name = urlFilename
	}
	return name
}

// Set Content-Disposition to arbitrary filename and disposition
func setContentDispositionHeader(w http.ResponseWriter, filename string, disposition string) {
	utf8Name := url.PathEscape(filename)
	asciiName := url.PathEscape(onlyASCII.ReplaceAllLiteralString(filename, "_"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s", disposition, asciiName, utf8Name))
}

// Set X-Ipfs-Roots with logical CID array for efficient HTTP cache invalidation.
func (i *handler) buildIpfsRootsHeader(contentPath string, r *http.Request) (string, error) {
	/*
		These are logical roots where each CID represent one path segment
		and resolves to either a directory or the root block of a file.
		The main purpose of this header is allow HTTP caches to do smarter decisions
		around cache invalidation (eg. keep specific subdirectory/file if it did not change)

		A good example is Wikipedia, which is HAMT-sharded, but we only care about
		logical roots that represent each segment of the human-readable content
		path:

		Given contentPath = /ipns/en.wikipedia-on-ipfs.org/wiki/Block_of_Wikipedia_in_Turkey
		rootCidList is a generated by doing `ipfs resolve -r` on each sub path:
			/ipns/en.wikipedia-on-ipfs.org → bafybeiaysi4s6lnjev27ln5icwm6tueaw2vdykrtjkwiphwekaywqhcjze
			/ipns/en.wikipedia-on-ipfs.org/wiki/ → bafybeihn2f7lhumh4grizksi2fl233cyszqadkn424ptjajfenykpsaiw4
			/ipns/en.wikipedia-on-ipfs.org/wiki/Block_of_Wikipedia_in_Turkey → bafkreibn6euazfvoghepcm4efzqx5l3hieof2frhp254hio5y7n3hv5rma

		The result is an ordered array of values:
			X-Ipfs-Roots: bafybeiaysi4s6lnjev27ln5icwm6tueaw2vdykrtjkwiphwekaywqhcjze,bafybeihn2f7lhumh4grizksi2fl233cyszqadkn424ptjajfenykpsaiw4,bafkreibn6euazfvoghepcm4efzqx5l3hieof2frhp254hio5y7n3hv5rma

		Note that while the top one will change every time any article is changed,
		the last root (responsible for specific article) may not change at all.
	*/
	var sp strings.Builder
	var pathRoots []string
	pathSegments := strings.Split(contentPath[6:], "/")
	sp.WriteString(contentPath[:5]) // /ipfs or /ipns
	for _, root := range pathSegments {
		if root == "" {
			continue
		}
		sp.WriteString("/")
		sp.WriteString(root)
		resolvedSubPath, err := i.api.ResolvePath(r.Context(), ipath.New(sp.String()))
		if err != nil {
			return "", err
		}
		pathRoots = append(pathRoots, resolvedSubPath.Cid().String())
	}
	rootCidList := strings.Join(pathRoots, ",") // convention from rfc2616#sec4.2
	return rootCidList, nil
}

func webRequestError(w http.ResponseWriter, err *requestError) {
	webError(w, err.Err, err.StatusCode)
}

func webError(w http.ResponseWriter, err error, defaultCode int) {
	switch {
	case isErrNotFound(err):
		webErrorWithCode(w, err, http.StatusNotFound)
	case errors.Is(err, ErrGatewayTimeout):
		webErrorWithCode(w, err, http.StatusGatewayTimeout)
	case errors.Is(err, ErrBadGateway):
		webErrorWithCode(w, err, http.StatusBadGateway)
	case errors.Is(err, context.DeadlineExceeded):
		webErrorWithCode(w, err, http.StatusGatewayTimeout)
	default:
		webErrorWithCode(w, err, defaultCode)
	}
}

func isErrNotFound(err error) bool {
	if ipld.IsNotFound(err) {
		return true
	}

	// Checks if err is a resolver.ErrNoLink. resolver.ErrNoLink does not implement
	// the .Is interface and cannot be directly compared to. Therefore, errors.Is
	// always returns false with it.
	for {
		_, ok := err.(resolver.ErrNoLink)
		if ok {
			return true
		}

		err = errors.Unwrap(err)
		if err == nil {
			return false
		}
	}
}

func webErrorWithCode(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
	if code >= 500 {
		log.Warnf("server error: %s", err)
	}
}

func getFilename(contentPath ipath.Path) string {
	s := contentPath.String()
	if (strings.HasPrefix(s, ipfsPathPrefix) || strings.HasPrefix(s, ipnsPathPrefix)) && strings.Count(gopath.Clean(s), "/") <= 2 {
		// Don't want to treat ipfs.io in /ipns/ipfs.io as a filename.
		return ""
	}
	return gopath.Base(s)
}

// etagMatch evaluates if we can respond with HTTP 304 Not Modified
// It supports multiple weak and strong etags passed in If-None-Matc stringh
// including the wildcard one.
func etagMatch(ifNoneMatchHeader string, cidEtag string, dirEtag string) bool {
	buf := ifNoneMatchHeader
	for {
		buf = textproto.TrimString(buf)
		if len(buf) == 0 {
			break
		}
		if buf[0] == ',' {
			buf = buf[1:]
			continue
		}
		// If-None-Match: * should match against any etag
		if buf[0] == '*' {
			return true
		}
		etag, remain := scanETag(buf)
		if etag == "" {
			break
		}
		// Check for match both strong and weak etags
		if etagWeakMatch(etag, cidEtag) || etagWeakMatch(etag, dirEtag) {
			return true
		}
		buf = remain
	}
	return false
}

// scanETag determines if a syntactically valid ETag is present at s. If so,
// the ETag and remaining text after consuming ETag is returned. Otherwise,
// it returns "", "".
// (This is the same logic as one executed inside of http.ServeContent)
func scanETag(s string) (etag string, remain string) {
	s = textproto.TrimString(s)
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s[start:]) < 2 || s[start] != '"' {
		return "", ""
	}
	// ETag is either W/"text" or "text".
	// See RFC 7232 2.3.
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		// Character values allowed in ETags.
		case c == 0x21 || c >= 0x23 && c <= 0x7E || c >= 0x80:
		case c == '"':
			return s[:i+1], s[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

// etagWeakMatch reports whether a and b match using weak ETag comparison.
func etagWeakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// generate Etag value based on HTTP request and CID
func getEtag(r *http.Request, cid cid.Cid) string {
	prefix := `"`
	suffix := `"`
	responseFormat, _, err := customResponseFormat(r)
	if err == nil && responseFormat != "" {
		// application/vnd.ipld.foo → foo
		// application/x-bar → x-bar
		shortFormat := responseFormat[strings.LastIndexAny(responseFormat, "/.")+1:]
		// Etag: "cid.shortFmt" (gives us nice compression together with Content-Disposition in block (raw) and car responses)
		suffix = `.` + shortFormat + suffix
	}
	// TODO: include selector suffix when https://github.com/ipfs/kubo/issues/8769 lands
	return prefix + cid.String() + suffix
}

// return explicit response format if specified in request as query parameter or via Accept HTTP header
func customResponseFormat(r *http.Request) (mediaType string, params map[string]string, err error) {
	if formatParam := r.URL.Query().Get("format"); formatParam != "" {
		// translate query param to a content type
		switch formatParam {
		case "raw":
			return "application/vnd.ipld.raw", nil, nil
		case "car":
			return "application/vnd.ipld.car", nil, nil
		case "tar":
			return "application/x-tar", nil, nil
		case "json":
			return "application/json", nil, nil
		case "cbor":
			return "application/cbor", nil, nil
		case "dag-json":
			return "application/vnd.ipld.dag-json", nil, nil
		case "dag-cbor":
			return "application/vnd.ipld.dag-cbor", nil, nil
		case "ipns-record":
			return "application/vnd.ipfs.ipns-record", nil, nil
		}
	}
	// Browsers and other user agents will send Accept header with generic types like:
	// Accept:text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8
	// We only care about explicit, vendor-specific content-types and respond to the first match (in order).
	// TODO: make this RFC compliant and respect weights (eg. return CAR for Accept:application/vnd.ipld.dag-json;q=0.1,application/vnd.ipld.car;q=0.2)
	for _, header := range r.Header.Values("Accept") {
		for _, value := range strings.Split(header, ",") {
			accept := strings.TrimSpace(value)
			// respond to the very first matching content type
			if strings.HasPrefix(accept, "application/vnd.ipld") ||
				strings.HasPrefix(accept, "application/x-tar") ||
				strings.HasPrefix(accept, "application/json") ||
				strings.HasPrefix(accept, "application/cbor") ||
				strings.HasPrefix(accept, "application/vnd.ipfs") {
				mediatype, params, err := mime.ParseMediaType(accept)
				if err != nil {
					return "", nil, err
				}
				return mediatype, params, nil
			}
		}
	}
	// If none of special-cased content types is found, return empty string
	// to indicate default, implicit UnixFS response should be prepared
	return "", nil, nil
}

// returns unquoted path with all special characters revealed as \u codes
func debugStr(path string) string {
	q := fmt.Sprintf("%+q", path)
	if len(q) >= 3 {
		q = q[1 : len(q)-1]
	}
	return q
}

// Resolve the provided contentPath including any special handling related to
// the requested responseFormat. Returned ok flag indicates if gateway handler
// should continue processing the request.
func (i *handler) handlePathResolution(w http.ResponseWriter, r *http.Request, responseFormat string, contentPath ipath.Path, logger *zap.SugaredLogger) (resolvedPath ipath.Resolved, newContentPath ipath.Path, ok bool) {
	// Attempt to resolve the provided path.
	resolvedPath, err := i.api.ResolvePath(r.Context(), contentPath)

	switch err {
	case nil:
		return resolvedPath, contentPath, true
	case coreiface.ErrOffline:
		err = fmt.Errorf("failed to resolve %s: %w", debugStr(contentPath.String()), err)
		webError(w, err, http.StatusServiceUnavailable)
		return nil, nil, false
	default:
		// The path can't be resolved.
		if isUnixfsResponseFormat(responseFormat) {
			// If we have origin isolation (subdomain gw, DNSLink website),
			// and response type is UnixFS (default for website hosting)
			// check for presence of _redirects file and apply rules defined there.
			// See: https://github.com/ipfs/specs/pull/290
			if hasOriginIsolation(r) {
				resolvedPath, newContentPath, ok, hadMatchingRule := i.serveRedirectsIfPresent(w, r, resolvedPath, contentPath, logger)
				if hadMatchingRule {
					logger.Debugw("applied a rule from _redirects file")
					return resolvedPath, newContentPath, ok
				}
			}

			// if Accept is text/html, see if ipfs-404.html is present
			// This logic isn't documented and will likely be removed at some point.
			// Any 404 logic in _redirects above will have already run by this time, so it's really an extra fall back
			if i.serveLegacy404IfPresent(w, r, contentPath) {
				logger.Debugw("served legacy 404")
				return nil, nil, false
			}
		}

		err = fmt.Errorf("failed to resolve %s: %w", debugStr(contentPath.String()), err)
		webError(w, err, http.StatusInternalServerError)
		return nil, nil, false
	}
}

// Detect 'Cache-Control: only-if-cached' in request and return data if it is already in the local datastore.
// https://github.com/ipfs/specs/blob/main/http-gateways/PATH_GATEWAY.md#cache-control-request-header
func (i *handler) handleOnlyIfCached(w http.ResponseWriter, r *http.Request, contentPath ipath.Path, logger *zap.SugaredLogger) (requestHandled bool) {
	if r.Header.Get("Cache-Control") == "only-if-cached" {
		if !i.api.IsCached(r.Context(), contentPath) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusPreconditionFailed)
				return true
			}
			errMsg := fmt.Sprintf("%q not in local datastore", contentPath.String())
			http.Error(w, errMsg, http.StatusPreconditionFailed)
			return true
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return true
		}
	}
	return false
}

func handleUnsupportedHeaders(r *http.Request) (err *requestError) {
	// X-Ipfs-Gateway-Prefix was removed (https://github.com/ipfs/kubo/issues/7702)
	// TODO: remove this after  go-ipfs 0.13 ships
	if prfx := r.Header.Get("X-Ipfs-Gateway-Prefix"); prfx != "" {
		err := fmt.Errorf("unsupported HTTP header: X-Ipfs-Gateway-Prefix support was removed: https://github.com/ipfs/kubo/issues/7702")
		return newRequestError(err, http.StatusBadRequest)
	}
	return nil
}

// ?uri query param support for requests produced by web browsers
// via navigator.registerProtocolHandler Web API
// https://developer.mozilla.org/en-US/docs/Web/API/Navigator/registerProtocolHandler
// TLDR: redirect /ipfs/?uri=ipfs%3A%2F%2Fcid%3Fquery%3Dval to /ipfs/cid?query=val
func handleProtocolHandlerRedirect(w http.ResponseWriter, r *http.Request, logger *zap.SugaredLogger) (requestHandled bool) {
	if uriParam := r.URL.Query().Get("uri"); uriParam != "" {
		u, err := url.Parse(uriParam)
		if err != nil {
			webError(w, fmt.Errorf("failed to parse uri query parameter: %w", err), http.StatusBadRequest)
			return true
		}
		if u.Scheme != "ipfs" && u.Scheme != "ipns" {
			webError(w, fmt.Errorf("uri query parameter scheme must be ipfs or ipns: %w", err), http.StatusBadRequest)
			return true
		}
		path := u.Path
		if u.RawQuery != "" { // preserve query if present
			path = path + "?" + u.RawQuery
		}

		redirectURL := gopath.Join("/", u.Scheme, u.Host, path)
		logger.Debugw("uri param, redirect", "to", redirectURL, "status", http.StatusMovedPermanently)
		http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
		return true
	}

	return false
}

// Disallow Service Worker registration on namespace roots
// https://github.com/ipfs/kubo/issues/4025
func handleServiceWorkerRegistration(r *http.Request) (err *requestError) {
	if r.Header.Get("Service-Worker") == "script" {
		matched, _ := regexp.MatchString(`^/ip[fn]s/[^/]+$`, r.URL.Path)
		if matched {
			err := fmt.Errorf("registration is not allowed for this scope")
			return newRequestError(fmt.Errorf("navigator.serviceWorker: %w", err), http.StatusBadRequest)
		}
	}

	return nil
}

// Attempt to fix redundant /ipfs/ namespace as long as resulting
// 'intended' path is valid.  This is in case gremlins were tickled
// wrong way and user ended up at /ipfs/ipfs/{cid} or /ipfs/ipns/{id}
// like in bafybeien3m7mdn6imm425vc2s22erzyhbvk5n3ofzgikkhmdkh5cuqbpbq :^))
func handleSuperfluousNamespace(w http.ResponseWriter, r *http.Request, contentPath ipath.Path) (requestHandled bool) {
	// If the path is valid, there's nothing to do
	if pathErr := contentPath.IsValid(); pathErr == nil {
		return false
	}

	// If there's no superflous namespace, there's nothing to do
	if !(strings.HasPrefix(r.URL.Path, "/ipfs/ipfs/") || strings.HasPrefix(r.URL.Path, "/ipfs/ipns/")) {
		return false
	}

	// Attempt to fix the superflous namespace
	intendedPath := ipath.New(strings.TrimPrefix(r.URL.Path, "/ipfs"))
	if err := intendedPath.IsValid(); err != nil {
		webError(w, fmt.Errorf("invalid ipfs path: %w", err), http.StatusBadRequest)
		return true
	}
	intendedURL := intendedPath.String()
	if r.URL.RawQuery != "" {
		// we render HTML, so ensure query entries are properly escaped
		q, _ := url.ParseQuery(r.URL.RawQuery)
		intendedURL = intendedURL + "?" + q.Encode()
	}
	// return HTTP 400 (Bad Request) with HTML error page that:
	// - points at correct canonical path via <link> header
	// - displays human-readable error
	// - redirects to intendedURL after a short delay

	w.WriteHeader(http.StatusBadRequest)
	if err := redirectTemplate.Execute(w, redirectTemplateData{
		RedirectURL:   intendedURL,
		SuggestedPath: intendedPath.String(),
		ErrorMsg:      fmt.Sprintf("invalid path: %q should be %q", r.URL.Path, intendedPath.String()),
	}); err != nil {
		webError(w, fmt.Errorf("failed to redirect when fixing superfluous namespace: %w", err), http.StatusBadRequest)
	}

	return true
}

func (i *handler) handleGettingFirstBlock(r *http.Request, begin time.Time, contentPath ipath.Path, resolvedPath ipath.Resolved) *requestError {
	// Update the global metric of the time it takes to read the final root block of the requested resource
	// NOTE: for legacy reasons this happens before we go into content-type specific code paths
	_, err := i.api.GetBlock(r.Context(), resolvedPath.Cid())
	if err != nil {
		err = fmt.Errorf("could not get block %s: %w", resolvedPath.Cid().String(), err)
		return newRequestError(err, http.StatusInternalServerError)
	}
	ns := contentPath.Namespace()
	timeToGetFirstContentBlock := time.Since(begin).Seconds()
	i.unixfsGetMetric.WithLabelValues(ns).Observe(timeToGetFirstContentBlock) // deprecated, use firstContentBlockGetMetric instead
	i.firstContentBlockGetMetric.WithLabelValues(ns).Observe(timeToGetFirstContentBlock)
	return nil
}

func (i *handler) setCommonHeaders(w http.ResponseWriter, r *http.Request, contentPath ipath.Path) *requestError {
	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("X-Ipfs-Path", contentPath.String())

	if rootCids, err := i.buildIpfsRootsHeader(contentPath.String(), r); err == nil {
		w.Header().Set("X-Ipfs-Roots", rootCids)
	} else { // this should never happen, as we resolved the contentPath already
		err = fmt.Errorf("error while resolving X-Ipfs-Roots: %w", err)
		return newRequestError(err, http.StatusInternalServerError)
	}

	return nil
}

// spanTrace starts a new span using the standard IPFS tracing conventions.
func spanTrace(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return otel.Tracer("go-libipfs").Start(ctx, fmt.Sprintf("%s.%s", " Gateway", spanName), opts...)
}

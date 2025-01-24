// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24

package http3

import (
	"io"
	"net/http"
	"strconv"

	"golang.org/x/net/internal/httpcommon"
)

// RoundTrip sends a request on the connection.
func (cc *ClientConn) RoundTrip(req *http.Request) (_ *http.Response, err error) {
	// Each request gets its own QUIC stream.
	st, err := newConnStream(req.Context(), cc.qconn, streamTypeRequest)
	if err != nil {
		return nil, err
	}
	defer func() {
		switch e := err.(type) {
		case nil:
		case *connectionError:
			cc.abort(e)
		case *streamError:
			st.stream.CloseRead()
			st.stream.Reset(uint64(e.code))
		default:
			st.stream.CloseRead()
			st.stream.Reset(uint64(errH3NoError))
		}
	}()

	// Cancel reads/writes on the stream when the request expires.
	st.stream.SetReadContext(req.Context())
	st.stream.SetWriteContext(req.Context())

	var encr httpcommon.EncodeHeadersResult
	headers := cc.enc.encode(func(yield func(itype indexType, name, value string)) {
		encr, err = httpcommon.EncodeHeaders(httpcommon.EncodeHeadersParam{
			Request:               req,
			AddGzipHeader:         false, // TODO: add when appropriate
			PeerMaxHeaderListSize: 0,
			DefaultUserAgent:      "Go-http-client/3",
		}, func(name, value string) {
			// Issue #71374: Consider supporting never-indexed fields.
			yield(mayIndex, name, value)
		})
	})
	if err != nil {
		return nil, err
	}

	// Write the HEADERS frame.
	st.writeVarint(int64(frameTypeHeaders))
	st.writeVarint(int64(len(headers)))
	st.Write(headers)
	if err := st.Flush(); err != nil {
		return nil, err
	}

	if encr.HasBody {
		// TODO: Send the request body.
	}

	// Read the response headers.
	for {
		ftype, err := st.readFrameHeader()
		if err != nil {
			return nil, err
		}
		switch ftype {
		case frameTypeHeaders:
			statusCode, h, err := cc.handleHeaders(st)
			if err != nil {
				return nil, err
			}

			if statusCode >= 100 && statusCode < 199 {
				// TODO: Handle 1xx responses.
				continue
			}

			// We have the response headers.
			// Set up the response and return it to the caller.
			contentLength, err := parseResponseContentLength(req.Method, statusCode, h)
			if err != nil {
				return nil, err
			}
			resp := &http.Response{
				Proto:         "HTTP/3.0",
				ProtoMajor:    3,
				Header:        h,
				StatusCode:    statusCode,
				Status:        strconv.Itoa(statusCode) + " " + http.StatusText(statusCode),
				ContentLength: contentLength,
				Body:          io.NopCloser(nil), // TODO: read the response body
			}
			// TODO: Automatic Content-Type: gzip decoding.
			return resp, nil
		case frameTypePushPromise:
			if err := cc.handlePushPromise(st); err != nil {
				return nil, err
			}
		default:
			if err := st.discardUnknownFrame(ftype); err != nil {
				return nil, err
			}
		}
	}
}

func parseResponseContentLength(method string, statusCode int, h http.Header) (int64, error) {
	clens := h["Content-Length"]
	if len(clens) == 0 {
		return -1, nil
	}

	// We allow duplicate Content-Length headers,
	// but only if they all have the same value.
	for _, v := range clens[1:] {
		if clens[0] != v {
			return -1, &streamError{errH3MessageError, "mismatching Content-Length headers"}
		}
	}

	// "A server MUST NOT send a Content-Length header field in any response
	// with a status code of 1xx (Informational) or 204 (No Content).
	// A server MUST NOT send a Content-Length header field in any 2xx (Successful)
	// response to a CONNECT request [...]"
	// https://www.rfc-editor.org/rfc/rfc9110#section-8.6-8
	if (statusCode >= 100 && statusCode < 200) ||
		statusCode == 204 ||
		(method == "CONNECT" && statusCode >= 200 && statusCode < 300) {
		// This is a protocol violation, but a fairly harmless one.
		// Just ignore the header.
		return -1, nil
	}

	contentLen, err := strconv.ParseUint(clens[0], 10, 63)
	if err != nil {
		return -1, &streamError{errH3MessageError, "invalid Content-Length header"}
	}
	return int64(contentLen), nil
}

func (cc *ClientConn) handleHeaders(st *stream) (statusCode int, h http.Header, err error) {
	haveStatus := false
	cookie := ""
	// Issue #71374: Consider tracking the never-indexed status of headers
	// with the N bit set in their QPACK encoding.
	err = cc.dec.decode(st, func(_ indexType, name, value string) error {
		switch {
		case name == ":status":
			if haveStatus {
				return &streamError{errH3MessageError, "duplicate :status"}
			}
			haveStatus = true
			statusCode, err = strconv.Atoi(value)
			if err != nil {
				return &streamError{errH3MessageError, "invalid :status"}
			}
		case name[0] == ':':
			// "Endpoints MUST treat a request or response
			// that contains undefined or invalid
			// pseudo-header fields as malformed."
			// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.3-3
			return &streamError{errH3MessageError, "undefined pseudo-header"}
		case name == "cookie":
			// "If a decompressed field section contains multiple cookie field lines,
			// these MUST be concatenated into a single byte string [...]"
			// using the two-byte delimiter of "; "''
			// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.2.1-2
			if cookie == "" {
				cookie = value
			} else {
				cookie += "; " + value
			}
		default:
			if h == nil {
				h = make(http.Header)
			}
			// TODO: Use a per-connection canonicalization cache as we do in HTTP/2.
			// Maybe we could put this in the QPACK decoder and have it deliver
			// pre-canonicalized headers to us here?
			cname := httpcommon.CanonicalHeader(name)
			// TODO: Consider using a single []string slice for all headers,
			// as we do in the HTTP/1 and HTTP/2 cases.
			// This is a bit tricky, since we don't know the number of headers
			// at the start of decoding. Perhaps it's worth doing a two-pass decode,
			// or perhaps we should just allocate header value slices in
			// reasonably-sized chunks.
			h[cname] = append(h[cname], value)
		}
		return nil
	})
	if !haveStatus {
		// "[The :status] pseudo-header field MUST be included in all responses [...]"
		// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.3.2-1
		err = errH3MessageError
	}
	if cookie != "" {
		if h == nil {
			h = make(http.Header)
		}
		h["Cookie"] = []string{cookie}
	}
	if err := st.endFrame(); err != nil {
		return 0, nil, err
	}
	return statusCode, h, err
}

func (cc *ClientConn) handlePushPromise(st *stream) error {
	// "A client MUST treat receipt of a PUSH_PROMISE frame that contains a
	// larger push ID than the client has advertised as a connection error of H3_ID_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.5-5
	return &connectionError{
		code:    errH3IDError,
		message: "PUSH_PROMISE received when no MAX_PUSH_ID has been sent",
	}
}

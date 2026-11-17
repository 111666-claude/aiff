# aiff codec

[![GoDoc](https://godoc.org/github.com/go-audio/aiff?status.svg)](https://godoc.org/github.com/go-audio/aiff)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-audio/aiff)](https://goreportcard.com/report/github.com/go-audio/aiff)
[![Coverage Status](https://codecov.io/gh/go-audio/aiff/graph/badge.svg)](https://codecov.io/gh/go-audio/aiff)
[![Build Status](https://travis-ci.org/go-audio/aiff.svg)](https://travis-ci.org/go-audio/aiff)

Support for int PCM data wrapped in aiff format.
AIFC/sowt format supported.
AIFC G.711 μ-law ("ulaw"/"ULAW") decoding and encoding is supported: 8 bit
code words decode to 16 bit linear PCM and 16 bit PCM can be written as a
μ-law AIFC file.

See [GoDoc](https://godoc.org/github.com/go-audio/aiff) for more details and examples.

/*
 * Copyright (c) 2016, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package common

import (
	"bytes"
	"compress/zlib"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
)

// AuthenticatedDataPackage is a JSON record containing some Psiphon data
// payload, such as list of Psiphon server entries. As it may be downloaded
// from various sources, it is digitally signed so that the data may be
// authenticated.
type AuthenticatedDataPackage struct {
	Data                   string `json:"data"`
	SigningPublicKeyDigest []byte `json:"signingPublicKeyDigest"`
	Signature              []byte `json:"signature"`
}

// GenerateAuthenticatedDataPackageKeys generates a key pair
// be used to sign and verify AuthenticatedDataPackages.
func GenerateAuthenticatedDataPackageKeys() (string, string, error) {

	rsaKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", ContextError(err)
	}

	publicKeyBytes, err := x509.MarshalPKIXPublicKey(rsaKey.Public())
	if err != nil {
		return "", "", ContextError(err)
	}

	privateKeyBytes := x509.MarshalPKCS1PrivateKey(rsaKey)

	return base64.StdEncoding.EncodeToString(publicKeyBytes),
		base64.StdEncoding.EncodeToString(privateKeyBytes),
		nil
}

func sha256sum(data string) []byte {
	digest := sha256.Sum256([]byte(data))
	return digest[:]
}

// WriteAuthenticatedDataPackage creates an AuthenticatedDataPackage
// containing the specified data and signed by the given key. The output
// conforms with the legacy format here:
// https://bitbucket.org/psiphon/psiphon-circumvention-system/src/c25d080f6827b141fe637050ce0d5bd0ae2e9db5/Automation/psi_ops_crypto_tools.py
func WriteAuthenticatedDataPackage(
	data string, signingPublicKey, signingPrivateKey string) ([]byte, error) {

	derEncodedPrivateKey, err := base64.StdEncoding.DecodeString(signingPrivateKey)
	if err != nil {
		return nil, ContextError(err)
	}
	rsaPrivateKey, err := x509.ParsePKCS1PrivateKey(derEncodedPrivateKey)
	if err != nil {
		return nil, ContextError(err)
	}

	signature, err := rsa.SignPKCS1v15(
		rand.Reader,
		rsaPrivateKey,
		crypto.SHA256,
		sha256sum(data))
	if err != nil {
		return nil, ContextError(err)
	}

	packageJSON, err := json.Marshal(
		&AuthenticatedDataPackage{
			Data: data,
			SigningPublicKeyDigest: sha256sum(signingPublicKey),
			Signature:              signature,
		})
	if err != nil {
		return nil, ContextError(err)
	}

	return Compress(packageJSON), nil
}

// ReadAuthenticatedDataPackage extracts and verifies authenticated
// data from an AuthenticatedDataPackage. The package must have been
// signed with the given key.
func ReadAuthenticatedDataPackage(
	compressedPackage []byte, signingPublicKey string) (string, error) {

	packageJSON, err := Decompress(compressedPackage)
	if err != nil {
		return "", ContextError(err)
	}

	var authenticatedDataPackage *AuthenticatedDataPackage
	err = json.Unmarshal(packageJSON, &authenticatedDataPackage)
	if err != nil {
		return "", ContextError(err)
	}

	derEncodedPublicKey, err := base64.StdEncoding.DecodeString(signingPublicKey)
	if err != nil {
		return "", ContextError(err)
	}
	publicKey, err := x509.ParsePKIXPublicKey(derEncodedPublicKey)
	if err != nil {
		return "", ContextError(err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return "", ContextError(errors.New("unexpected signing public key type"))
	}

	if 0 != bytes.Compare(
		authenticatedDataPackage.SigningPublicKeyDigest,
		sha256sum(signingPublicKey)) {

		return "", ContextError(errors.New("unexpected signing public key digest"))
	}

	err = rsa.VerifyPKCS1v15(
		rsaPublicKey,
		crypto.SHA256,
		sha256sum(authenticatedDataPackage.Data),
		authenticatedDataPackage.Signature)
	if err != nil {
		return "", ContextError(err)
	}

	return authenticatedDataPackage.Data, nil
}

// StreamingReadAuthenticatedDataPackage extracts and verifies authenticated
// data from an AuthenticatedDataPackage stored in the specified file. The
// package must have been signed with the given key.
// StreamingReadAuthenticatedDataPackage does not load the entire package nor
// the entire data into memory. It streams the package while verifying, and
// returns an io.ReadCloser that the caller may use to stream the authenticated
// data payload. The caller _must_ close the io.Closer to free resources and
// close the underlying file.
func StreamingReadAuthenticatedDataPackage(
	packageFileName string, signingPublicKey string) (io.ReadCloser, error) {

	file, err := os.Open(packageFileName)
	if err != nil {
		return nil, ContextError(err)
	}

	closeOnError := file
	defer func() {
		if closeOnError != nil {
			closeOnError.Close()
		}
	}()

	var payload io.ReadCloser

	// The file is streamed in 2 passes. The first pass verifies the package
	// signature. No payload data should be accepted/processed until the signature
	// check is complete. The second pass repositions to the data payload and returns
	// a reader the caller will use to stream the authenticated payload.
	//
	// Note: No exclusive file lock is held between passes, so it's possible to
	// verify the data in one pass, and read different data in the second pass.
	// For Psiphon's use cases, this will not happen in practise -- the packageFileName
	// will not change while StreamingReadAuthenticatedDataPackage is running -- unless
	// the client host is compromised; a compromised client host is outside of our threat
	// model.

	for pass := 0; pass < 2; pass++ {

		_, err = file.Seek(0, 0)
		if err != nil {
			return nil, ContextError(err)
		}

		decompressor, err := zlib.NewReader(file)
		if err != nil {
			return nil, ContextError(err)
		}
		// TODO: need to Close decompressor to ensure zlib checksum is verified?

		hash := sha256.New()

		var jsonData io.Reader
		var jsonSigningPublicKey []byte
		var jsonSignature []byte

		jsonReadBase64Value := func(value io.Reader) ([]byte, error) {
			base64Value, err := ioutil.ReadAll(value)
			if err != nil {
				return nil, ContextError(err)
			}
			decodedValue, err := base64.StdEncoding.DecodeString(string(base64Value))
			if err != nil {
				return nil, ContextError(err)
			}
			return decodedValue, nil
		}

		jsonHandler := func(key string, value io.Reader) (bool, error) {
			switch key {

			case "data":
				if pass == 0 {

					_, err := io.Copy(hash, value)
					if err != nil {
						return false, ContextError(err)
					}
					return true, nil

				} else { // pass == 1

					jsonData = value

					// The JSON stream parser must halt at this position,
					// leaving the reader to be returned to the caller positioned
					// at the start of the data payload.
					return false, nil
				}

			case "signingPublicKeyDigest":
				jsonSigningPublicKey, err = jsonReadBase64Value(value)
				if err != nil {
					return false, ContextError(err)
				}
				return true, nil

			case "signature":
				jsonSignature, err = jsonReadBase64Value(value)
				if err != nil {
					return false, ContextError(err)
				}
				return true, nil
			}

			return false, ContextError(fmt.Errorf("unexpected key '%s'", key))
		}

		jsonStreamer := &limitedJSONStreamer{
			reader:  decompressor,
			handler: jsonHandler,
		}

		err = jsonStreamer.Stream()
		if err != nil {
			return nil, ContextError(err)
		}

		if pass == 0 {

			if jsonSigningPublicKey == nil || jsonSignature == nil {
				return nil, ContextError(errors.New("missing expected field"))
			}

			derEncodedPublicKey, err := base64.StdEncoding.DecodeString(signingPublicKey)
			if err != nil {
				return nil, ContextError(err)
			}
			publicKey, err := x509.ParsePKIXPublicKey(derEncodedPublicKey)
			if err != nil {
				return nil, ContextError(err)
			}
			rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
			if !ok {
				return nil, ContextError(errors.New("unexpected signing public key type"))
			}

			if 0 != bytes.Compare(jsonSigningPublicKey, sha256sum(signingPublicKey)) {
				return nil, ContextError(errors.New("unexpected signing public key digest"))
			}

			err = rsa.VerifyPKCS1v15(
				rsaPublicKey,
				crypto.SHA256,
				hash.Sum(nil),
				jsonSignature)
			if err != nil {
				return nil, ContextError(err)
			}

		} else { // pass == 1

			if jsonData == nil {
				return nil, ContextError(errors.New("missing expected field"))
			}

			payload = struct {
				io.Reader
				io.Closer
			}{
				jsonData,
				file,
			}
		}
	}

	closeOnError = nil

	return payload, nil
}

// limitedJSONStreamer is a streaming JSON parser that supports just the
// JSON required for the AuthenticatedDataPackage format and expected data payloads.
//
// Unlike other common streaming JSON parsers, limitedJSONStreamer streams the JSON
// _values_, as the AuthenticatedDataPackage "data" value may be too large to fit into
// memory.
//
// limitedJSONStreamer is not intended for use outside of AuthenticatedDataPackage
// and supports only a small subset of JSON: one object with string values only,
// no escaped characters, no nested objects, no arrays, no numbers, etc.
//
// limitedJSONStreamer does support any JSON spec (http://www.json.org/) format
// for its limited subset. So, for example, any whitespace/formatting should be
// supported and the creator of AuthenticatedDataPackage should be able to use
// any valid JSON that results in a AuthenticatedDataPackage object.
//
// For each key/value pair, handler is invoked with the key name and a reader
// to stream the value. The handler _must_ read value to EOF (or return an error).
type limitedJSONStreamer struct {
	reader  io.Reader
	handler func(key string, value io.Reader) (bool, error)
}

const (
	stateJSONSeekingObjectStart = iota
	stateJSONSeekingKeyStart
	stateJSONSeekingKeyEnd
	stateJSONSeekingColon
	stateJSONSeekingStringValueStart
	stateJSONSeekingStringValueEnd
	stateJSONSeekingNextPair
	stateJSONObjectEnd
)

func (streamer *limitedJSONStreamer) Stream() error {

	// TODO: validate that strings are valid Unicode?

	isWhitespace := func(b byte) bool {
		return b == ' ' || b == '\t' || b == '\r' || b == '\n'
	}

	nextByte := make([]byte, 1)
	keyBuffer := new(bytes.Buffer)
	state := stateJSONSeekingObjectStart

	for {
		n, readErr := streamer.reader.Read(nextByte)

		if n > 0 {

			b := nextByte[0]

			switch state {

			case stateJSONSeekingObjectStart:
				if b == '{' {
					state = stateJSONSeekingKeyStart
				} else if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U while seeking object start", b))
				}

			case stateJSONSeekingKeyStart:
				if b == '"' {
					state = stateJSONSeekingKeyEnd
					keyBuffer.Reset()
				} else if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U while seeking key start", b))
				}

			case stateJSONSeekingKeyEnd:
				if b == '\\' {
					return ContextError(errors.New("unsupported escaped character"))
				} else if b == '"' {
					state = stateJSONSeekingColon
				} else {
					keyBuffer.WriteByte(b)
				}

			case stateJSONSeekingColon:
				if b == ':' {
					state = stateJSONSeekingStringValueStart
				} else if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U while seeking colon", b))
				}

			case stateJSONSeekingStringValueStart:
				if b == '"' {
					state = stateJSONSeekingStringValueEnd

					key := string(keyBuffer.Bytes())

					// Wrap the main reader in a reader that will read up to the end
					// of the value and then EOF. The handler is expected to consume
					// the full value, and then stream parsing will resume after the
					// end of the value.
					valueStreamer := &limitedJSONValueStreamer{
						reader: streamer.reader,
					}

					continueStreaming, err := streamer.handler(key, valueStreamer)
					if err != nil {
						return ContextError(err)
					}

					// The handler may request that streaming halt at this point; no
					// further changes are made to streamer.reader, leaving the value
					// exactly where the hander leaves it.
					if !continueStreaming {
						return nil
					}

					state = stateJSONSeekingNextPair

				} else if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U while seeking value start", b))
				}

			case stateJSONSeekingNextPair:
				if b == ',' {
					state = stateJSONSeekingKeyStart
				} else if b == '}' {
					state = stateJSONObjectEnd
				} else if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U while seeking next name/value pair", b))
				}

			case stateJSONObjectEnd:
				if !isWhitespace(b) {
					return ContextError(fmt.Errorf("unexpected character %#U after object end", b))
				}

			default:
				return ContextError(errors.New("unexpected state"))

			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				if state != stateJSONObjectEnd {
					return ContextError(errors.New("unexpected EOF before object end"))
				}
				return nil
			}
			return ContextError(readErr)
		}
	}
}

// limitedJSONValueStreamer wraps the limitedJSONStreamer reader
// with a reader that reads to the end of a string value and then
// terminates with EOF.
type limitedJSONValueStreamer struct {
	mutex  sync.Mutex
	eof    bool
	reader io.Reader
}

// Read implements the io.Reader interface.
func (streamer *limitedJSONValueStreamer) Read(p []byte) (int, error) {
	streamer.mutex.Lock()
	defer streamer.mutex.Unlock()

	if streamer.eof {
		return 0, io.EOF
	}

	var i int
	var err error

	for i = 0; i < len(p); i++ {

		var n int
		n, err = streamer.reader.Read(p[i : i+1])

		if n == 1 {
			if p[i] == '"' {
				n = 0
				streamer.eof = true
				err = io.EOF
			} else if p[i] == '\\' {
				n = 0
				err = ContextError(errors.New("unsupported escaped character"))
			}
		}

		if err != nil {
			break
		}
	}

	return i, err
}

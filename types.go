// Discordgo - Discord bindings for Go
// Available at https://github.com/bwmarrin/discordgo

// Copyright 2015-2016 Bruce Marriner <bruce@sqls.net>.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains custom types, currently only a timestamp wrapper.

package discordgo

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Timestamp stores a timestamp, as sent by the Discord API.
type Timestamp string

// Parse parses a timestamp string into a time.Time object.
// The only time this can fail is if Discord changes their timestamp format.
func (t Timestamp) Parse() (time.Time, error) {
	return time.Parse(time.RFC3339, string(t))
}

// RESTError stores error information about a request with a bad response code.
// Message is not always present, there are cases where api calls can fail
// without returning a json message.
type RESTError struct {
	Request      *http.Request
	Response     *http.Response
	ResponseBody []byte

	Message *APIErrorMessage // Message may be nil.
}

func newRestError(req *http.Request, resp *http.Response, body []byte) *RESTError {
	restErr := &RESTError{
		Request:      req,
		Response:     resp,
		ResponseBody: body,
	}

	// Attempt to decode the error and assume no message was provided if it fails
	var msg *APIErrorMessage
	err := json.Unmarshal(body, &msg)
	if err == nil {
		restErr.Message = msg
	}

	return restErr
}

func (r RESTError) Error() string {
	return fmt.Sprintf("HTTP %s, %s", r.Response.Status, r.ResponseBody)
}

type Bucket struct {
	sync.Mutex
	remaining int
	limit     int
	reset     time.Time
	r         *RateLimiter
}

// Release unlocks the bucket and reads the headers to update the bucket's ratelimit info
// to the relevant bucket or locks up the whole thing in case of a global
// ratelimit.
func (b *Bucket) Release(path string, headers http.Header) error {
	defer b.Mutex.Unlock()
	if headers == nil {
		log.Println("Null headers")
		return nil
	}

	remaining := headers.Get("X-RateLimit-Remaining")
	reset := headers.Get("X-RateLimit-Reset")
	global := headers.Get("X-RateLimit-Global")

	// If it's global just keep the main ratelimit mutex locked
	if global != "" {
		retryAfer, err := strconv.Atoi(headers.Get("Retry-After"))
		if err != nil {
			return err
		}

		go func() {
			b.r.Lock()
			time.Sleep(time.Millisecond * time.Duration(retryAfer))
			b.r.Unlock()
		}()

		log.Println("GLOBAL RATELIMIT", global)
		return nil
	}

	if reset == "" || remaining == "" {
		log.Println("RESET OR REMAINING EMPTY")
		return errors.New("No ratelimit headers provided")
	}

	parsedReset, err := strconv.ParseInt(reset, 10, 64)
	if err != nil {
		return err
	}

	parsedRemaining, err := strconv.ParseInt(remaining, 10, 32)
	if err != nil {
		return err
	}

	//Mase sure we don't acidentally increase it incase another request finished earlier
	b.remaining = int(parsedRemaining)
	b.reset = time.Unix(parsedReset, 0)

	log.Println(path, parsedReset)
	return nil
}

type RateLimiter struct {
	sync.Mutex
	buckets         map[string]*Bucket
	globalRateLimit time.Duration
}

func newRateLimiter() *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*Bucket),
	}
}

func (r *RateLimiter) getBucket(key string) *Bucket {
	if bucket, ok := r.buckets[key]; ok {
		return bucket
	}

	b := &Bucket{remaining: 1, r: r}
	r.buckets[key] = b
	return b
}

// Locks untill were allowed to make a request
func (r *RateLimiter) LockBucket(path string) *Bucket {
	r.Lock()
	b := r.getBucket(path)
	r.Unlock()

	b.Lock()

	// If we ran out of calls and the reset time is still ahead of us
	// Or if limit is set, and remaining is less than -limit, which means
	// that we exeeded the next amount of calls aswell
	for b.remaining < 1 && b.reset.After(time.Now()) {
		toSleep := b.reset.Sub(time.Now())
		time.Sleep(toSleep)

	}

	// Lock and unlock to check for global ratelimites after sleeping
	r.Lock()
	r.Unlock()

	b.remaining--
	log.Println(b.remaining)
	return b
}

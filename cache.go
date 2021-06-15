package cache

import (
	"bytes"
	"net/http"
	"sync"
	"time"

	"github.com/chenyahui/gin-cache/persist"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

type Options struct {
	// CacheStore the cache backend to store response
	CacheStore persist.CacheStore

	// CacheDuration
	CacheDuration time.Duration

	// DisableSingleFlight means whether use singleflight to avoid Hotspot Invalid when cache miss
	DisableSingleFlight bool

	// SingleflightForgetTime this option only be effective when DisableSingleFlight is false
	SingleflightForgetTime time.Duration

	// Logger
	Logger Logger
}

type Logger interface {
	Printf(format string, args ...interface{})
}

type Handler func(c *gin.Context) (string, bool)

// Cache user must pass getCacheKey to describe the way to generate cache key
func Cache(handler Handler, options Options) gin.HandlerFunc {
	if options.CacheStore == nil {
		panic("CacheStore can not be nil")
	}

	cacheHelper := newCacheHelper(options)

	return func(c *gin.Context) {
		cacheKey, needCache := handler(c)
		if !needCache {
			c.Next()
			return
		}

		// read cache first
		{
			respCache := cacheHelper.getResponseCache()
			defer cacheHelper.putResponseCache(respCache)

			err := options.CacheStore.Get(cacheKey, &respCache)
			if err == nil {
				cacheHelper.respondWithCache(c, respCache)
				return
			}

			if err != persist.ErrCacheMiss {
				if options.Logger != nil {
					options.Logger.Printf("get cache: %v", err)
				}
			}
		}

		// set context writer to cacheWriter in order to record the response
		cacheWriter := &responseCacheWriter{}
		cacheWriter.reset(c.Writer)
		c.Writer = cacheWriter

		respCache := &responseCache{}

		if options.DisableSingleFlight {
			c.Next()

			respCache.fill(cacheWriter)
		} else {
			// use singleflight to avoid Hotspot Invalid
			rawCacheWriter, _, shared := cacheHelper.sfGroup.Do(cacheKey, func() (interface{}, error) {
				if options.SingleflightForgetTime > 0 {
					go func() {
						time.Sleep(options.SingleflightForgetTime)
						cacheHelper.sfGroup.Forget(cacheKey)
					}()
				}

				c.Next()

				return cacheWriter, nil
			})

			cacheWriter = rawCacheWriter.(*responseCacheWriter)
			respCache.fill(cacheWriter)

			if shared {
				cacheHelper.respondWithCache(c, respCache)
			}
		}

		if err := options.CacheStore.Set(cacheKey, respCache, options.CacheDuration); err != nil {
			if options.Logger != nil {
				options.Logger.Printf("set cache error: %v", err)
			}
		}
	}
}

// CacheByURI a shortcut function for caching response with uri
func CacheByURI(options Options) gin.HandlerFunc {
	return Cache(
		func(c *gin.Context) (string, bool) {
			return c.Request.RequestURI, true
		},
		options,
	)
}

// CacheByPath a shortcut function for caching response with url path, discard the query params
func CacheByPath(options Options) gin.HandlerFunc {
	return Cache(
		func(c *gin.Context) (string, bool) {
			return c.Request.URL.Path, true
		},
		options,
	)
}

type responseCache struct {
	Status int
	Header http.Header
	Data   []byte
}

func (c *responseCache) reset() {
	c.Data = c.Data[0:0]
	c.Header = make(http.Header)
}

func (c *responseCache) fill(cacheWriter *responseCacheWriter) {
	c.Status = cacheWriter.Status()
	c.Data = cacheWriter.body.Bytes()
	c.Header = make(http.Header, len(cacheWriter.Header()))

	for key, value := range cacheWriter.Header() {
		c.Header[key] = value
	}
}

// responseCacheWriter
type responseCacheWriter struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *responseCacheWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *responseCacheWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func (w *responseCacheWriter) reset(writer gin.ResponseWriter) {
	w.body.Reset()
	w.ResponseWriter = writer
}

func newResponseCachePool() *sync.Pool {
	return &sync.Pool{
		New: func() interface{} {
			return &responseCache{
				Header: make(http.Header),
			}
		},
	}
}

type cacheHelper struct {
	sfGroup           singleflight.Group
	responseCachePool *sync.Pool
	options           Options
}

func newCacheHelper(options Options) *cacheHelper {
	return &cacheHelper{
		sfGroup:           singleflight.Group{},
		responseCachePool: newResponseCachePool(),
		options:           options,
	}
}

func (m *cacheHelper) getResponseCache() *responseCache {
	respCache := m.responseCachePool.Get().(*responseCache)
	respCache.reset()

	return respCache
}

func (m *cacheHelper) putResponseCache(c *responseCache) {
	m.responseCachePool.Put(c)
}

func (m *cacheHelper) respondWithCache(
	c *gin.Context,
	respCache *responseCache,
) {
	c.Writer.WriteHeader(respCache.Status)
	for k, vals := range respCache.Header {
		for _, v := range vals {
			c.Writer.Header().Set(k, v)
		}
	}

	if _, err := c.Writer.Write(respCache.Data); err != nil {
		if m.options.Logger != nil {
			m.options.Logger.Printf("write response error: %v", err)
		}
	}

	// abort handler chain and return directly
	c.Abort()
}

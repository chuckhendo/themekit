package themekit

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const CreateThemeMaxRetries int = 3

type ThemeClient struct {
	config Configuration
	client *http.Client
	filter EventFilter
}

type Theme struct {
	Name        string `json:"name"`
	Source      string `json:"src,omitempty"`
	Role        string `json:"role,omitempty"`
	Id          int64  `json:"id,omitempty"`
	Previewable bool   `json:"previewable,omitempty"`
}

type apiResponse struct {
	code int
	body []byte
	err  error
}

type EventType int

func (e EventType) String() string {
	switch e {
	case Update:
		return "Update"
	case Remove:
		return "Remove"
	default:
		return "Unknown"
	}
}

type NonFatalNetworkError struct {
	Code    int
	Verb    string
	Message string
}

func (e NonFatalNetworkError) Error() string {
	return fmt.Sprintf("%d %s %s", e.Code, e.Verb, e.Message)
}

const (
	Update EventType = iota
	Remove
)

type AssetEvent interface {
	Asset() Asset
	Type() EventType
}

func NewThemeClient(config Configuration) ThemeClient {
	return ThemeClient{
		config: config,
		client: newHttpClient(config),
		filter: NewEventFilterFromPatternsAndFiles(config.IgnoredFiles, config.Ignores),
	}
}

func (t ThemeClient) GetConfiguration() Configuration {
	return t.config
}

func (t ThemeClient) LeakyBucket() *LeakyBucket {
	return NewLeakyBucket(t.config.BucketSize, t.config.RefillRate, 1)
}

func (t ThemeClient) AssetList() (results chan Asset, errs chan error) {
	results = make(chan Asset)
	errs = make(chan error)
	go func() {
		queryBuilder := func(path string) string {
			return path
		}

		resp := t.query(queryBuilder)
		if resp.err != nil {
			errs <- resp.err
		}

		var assets map[string][]Asset
		err := json.Unmarshal(resp.body, &assets)
		if err != nil {
			errs <- err
			return
		}

		sort.Sort(ByAsset(assets["assets"]))
		sanitizedAssets := ignoreCompiledAssets(assets["assets"])

		for _, asset := range sanitizedAssets {
			results <- asset
		}
		close(results)
		close(errs)
	}()
	return
}

func (t ThemeClient) AssetListSync() []Asset {
	ch, _ := t.AssetList()
	results := []Asset{}
	for {
		asset, more := <-ch
		if !more {
			return results
		}
		results = append(results, asset)
	}
}

func (t ThemeClient) LocalAssets(dir string) []Asset {
	dir = fmt.Sprintf("%s%s", dir, string(filepath.Separator))
	glob := fmt.Sprintf("%s**%s*", dir, string(filepath.Separator))
	files, _ := filepath.Glob(glob)

	assets := []Asset{}
	for _, file := range files {
		if !t.filter.MatchesFilter(file) {
			assetKey := strings.Replace(file, dir, "", -1)
			asset, err := LoadAsset(dir, assetKey)
			if err == nil {
				assets = append(assets, asset)
			}
		}
	}
	return assets
}

type AssetRetrieval func(filename string) (Asset, error)

func (t ThemeClient) Asset(filename string) (Asset, error) {
	queryBuilder := func(path string) string {
		return fmt.Sprintf("%s&asset[key]=%s", path, filename)
	}

	resp := t.query(queryBuilder)
	if resp.err != nil {
		return Asset{}, resp.err
	}
	if resp.code >= 400 {
		return Asset{}, NonFatalNetworkError{Code: resp.code, Verb: "GET", Message: "not found"}
	}
	var asset map[string]Asset
	err := json.Unmarshal(resp.body, &asset)
	if err != nil {
		return Asset{}, err
	}

	return asset["asset"], nil
}

func (t ThemeClient) CreateTheme(name, zipLocation string) (ThemeClient, chan ThemeEvent) {
	var wg sync.WaitGroup
	wg.Add(1)
	path := fmt.Sprintf("%s/themes.json", t.config.AdminUrl())
	contents := map[string]Theme{
		"theme": Theme{Name: name, Source: zipLocation, Role: "unpublished"},
	}

	log := make(chan ThemeEvent)
	logEvent := func(t ThemeEvent) {
		log <- t
	}

	retries := 0
	themeEvent := func() (themeEvent APIThemeEvent) {
		ready := false
		data, _ := json.Marshal(contents)
		for retries < CreateThemeMaxRetries && !ready {
			if themeEvent = t.sendData("POST", path, data); !themeEvent.Successful() {
				retries++
			} else {
				ready = true
			}
			go logEvent(themeEvent)
		}
		if retries >= CreateThemeMaxRetries {
			err := errors.New(fmt.Sprintf("'%s' cannot be retrieved from Github.", zipLocation))
			NotifyError(err)
		}
		return
	}()

	go func() {
		for !t.isDoneProcessing(themeEvent.ThemeId) {
			time.Sleep(250 * time.Millisecond)
		}
		wg.Done()
	}()

	wg.Wait()
	config := t.GetConfiguration()
	config.ThemeId = themeEvent.ThemeId
	return NewThemeClient(config.Initialize()), log
}

func (t ThemeClient) Process(events chan AssetEvent) (done chan bool, messages chan ThemeEvent) {
	done = make(chan bool)
	messages = make(chan ThemeEvent)
	go func() {
		for {
			job, more := <-events
			if more {
				messages <- t.Perform(job)
			} else {
				close(messages)
				done <- true
				return
			}
		}
	}()
	return
}

func (t ThemeClient) Perform(asset AssetEvent) ThemeEvent {
	if t.filter.MatchesFilter(asset.Asset().Key) {
		return NoOpEvent{}
	}
	var event string
	switch asset.Type() {
	case Update:
		event = "PUT"
	case Remove:
		event = "DELETE"
	}
	resp, err := t.request(asset, event)
	if err == nil {
		defer resp.Body.Close()
	}
	return processResponse(resp, err, asset)
}

func (t ThemeClient) query(queryBuilder func(path string) string) apiResponse {
	path := fmt.Sprintf("%s?fields=key,attachment,value", t.config.AssetPath())
	path = queryBuilder(path)

	req, err := http.NewRequest("GET", path, nil)
	if err != nil {
		return apiResponse{err: err}
	}

	t.config.AddHeaders(req)
	resp, err := t.client.Do(req)
	if err != nil {
		return apiResponse{err: err}
	} else {
		defer resp.Body.Close()
	}
	body, err := ioutil.ReadAll(resp.Body)
	return apiResponse{code: resp.StatusCode, body: body, err: err}
}

func (t ThemeClient) sendData(method, path string, body []byte) (result APIThemeEvent) {
	req, err := http.NewRequest(method, path, bytes.NewBuffer(body))
	if err != nil {
		NotifyError(err)
	}
	t.config.AddHeaders(req)
	resp, err := t.client.Do(req)
	if result = NewAPIThemeEvent(resp, err); result.Successful() {
		defer resp.Body.Close()
	}
	return result
}

func (t ThemeClient) request(event AssetEvent, method string) (*http.Response, error) {
	path := t.config.AssetPath()
	data := map[string]Asset{"asset": event.Asset()}
	encoded, err := json.Marshal(data)

	req, err := http.NewRequest(method, path, bytes.NewBuffer(encoded))

	if err != nil {
		return nil, err
	}

	t.config.AddHeaders(req)
	return t.client.Do(req)
}

func processResponse(r *http.Response, err error, event AssetEvent) ThemeEvent {
	return NewAPIAssetEvent(r, event, err)
}

func (t ThemeClient) isDoneProcessing(themeId int64) bool {
	path := fmt.Sprintf("%s/themes/%d.json", t.config.AdminUrl(), themeId)
	themeEvent := t.sendData("GET", path, []byte{})
	return themeEvent.Previewable
}

func ExtractErrorMessage(data []byte, err error) string {
	return extractAssetAPIErrors(data, err).Error()
}

func newHttpClient(config Configuration) (client *http.Client) {
	client = &http.Client{}
	if len(config.Proxy) > 0 {
		fmt.Println("Proxy URL detected from Configuration:", config.Proxy)
		fmt.Println("SSL Certificate Validation will be disabled!")
		proxyUrl, err := url.Parse(config.Proxy)
		if err != nil {
			fmt.Println("Proxy configuration invalid:", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyUrl), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return
}

func ignoreCompiledAssets(assets []Asset) []Asset {
	newSize := 0
	results := make([]Asset, len(assets))
	isCompiled := func(a Asset, rest []Asset) bool {
		for _, other := range rest {
			if strings.Contains(other.Key, a.Key) {
				return true
			}
		}
		return false
	}
	for index, asset := range assets {
		if !isCompiled(asset, assets[index+1:]) {
			results[newSize] = asset
			newSize++
		}
	}
	return results[:newSize]
}

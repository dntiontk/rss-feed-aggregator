package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mmcdole/gofeed/rss"
	"gopkg.in/yaml.v3"
)

/*
This is a simple RSS feed aggregator for the City of Windsor website
that uses the Open Data feed to create a summary of changes. When
invoked, the program will fetch the the remote feed, and compare
it to a local copy. After generating and outputting the a summary
of changes, the remote copy will overwrite the local copy
(create if it doesn't exist). The invoker is responsible for keeping
 the local copy up-to-date.
*/

// FeedConfig contains the url, name and filepath of a given RSS feed
type FeedConfig struct {
	url  string
	name string
	path string
}

var (
	opendataFeed = FeedConfig{
		url:  "https://opendata.citywindsor.ca/RSS",
		name: "Open Data",
		path: "feeds/opendata.xml",
	}
)

//go:embed star.citywindsor.ca
var cert []byte

func main() {
	/*
		Note that we need to add the ca-cert for "citywindsor.ca" to
		to our HTTP client in order to access the data programatically
	*/
	client, err := newClientWithCA(cert)
	if err != nil {
		log.Fatal(err)
	}

	// Get our Open Data update list
	opendataUpdates, err := getFeedUpdates(client, &opendataFeed)
	if err != nil {
		log.Fatal(err)
	}

	// exit if no changes found
	if len(opendataUpdates) == 0 {
		log.Println("no changes found")
	} else {
		updates := make([]Update, 0)
		for _, u := range opendataUpdates {
			// only capture csv files for now
			if strings.Contains(u.Link, ".csv") {
				updates = append(updates, Update{
					Name: u.Title,
					Link: u.Link,
					Date: u.PubDateParsed,
				})
			}
		}
		date := time.Now().Format(DateOnly)
		summary := Post{
			Title:   fmt.Sprintf("Open Data Weekly Summary - %s", date),
			Date:    date,
			Author:  "rss-bot",
			Cover:   "",
			Tags:    []string{"rss", "opendata", "automation"},
			Updates: updates,
		}

		out, err := yaml.Marshal(summary)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(out))
	}
}

const (
	DateOnly = "2006-01-02"
)

type Update struct {
	Name string     `yaml:"name"`
	Link string     `yaml:"link"`
	Date *time.Time `yaml:"date"`
}

type Post struct {
	Title   string   `yaml:"title"`
	Date    string   `yaml:"date"`
	Author  string   `yaml:"author"`
	Cover   string   `yaml:"cover"`
	Tags    []string `yaml:"tags"`
	Updates []Update `yaml:"updates"`
}

func parseAllUpdates(items []*rss.Item) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	for _, i := range items {
		out = append(out, parseToUpdate(i))
	}
	return out
}

func parseToUpdate(item *rss.Item) map[string]interface{} {
	return map[string]interface{}{
		"title": item.Title,
		"link":  item.Link,
		"date":  *item.PubDateParsed,
	}
}

func getFeedUpdates(client *http.Client, fc *FeedConfig) ([]*rss.Item, error) {
	localFeed, err := fc.parseLocalFeed()
	if err != nil {
		log.Fatal(err)
	}

	/*
		Let's create a map[string]time.Time to quickly lookup items and
		compare dates
	*/
	itemMap := make(map[string]time.Time)

	for _, item := range localFeed.Items {
		formatted := item.PubDateParsed.Format(time.RFC3339)
		pubDate, err := time.Parse(time.RFC3339, formatted)
		if err != nil {
			return nil, fmt.Errorf("unable to parse date from local feed: %v", err)
		}
		itemMap[item.Title] = pubDate
	}

	// Parse the remote copy of the opendata feed
	remoteFeed, err := fc.parseRemoteFeed(client)
	if err != nil {
		return nil, fmt.Errorf("unable to parse remote feed: %v", err)
	}

	// Make updatedItems lists
	return lookupUpdates(itemMap, remoteFeed.Items)
}

func lookupUpdates(m map[string]time.Time, items []*rss.Item) ([]*rss.Item, error) {
	out := make([]*rss.Item, 0)
	for _, i := range items {
		if date, ok := m[i.Title]; ok {
			formatted := i.PubDateParsed.Format(time.RFC3339)
			rDate, err := time.Parse(time.RFC3339, formatted)
			if err != nil {
				return nil, err
			}
			if !rDate.Equal(date) {
				out = append(out, i)
			}
		} else {
			out = append(out, i)
		}
	}
	return out, nil
}

// newClientWithCA reads a CA cert as bytes and returns an HTTP client with the appropriate cert pool
func newClientWithCA(cert []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(cert); !ok {
		return nil, fmt.Errorf("unable to append ca to cert pool")
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}, nil
}

func (fc *FeedConfig) parseRemoteFeed(c *http.Client) (*rss.Feed, error) {
	resp, err := c.Get(fc.url)
	if err != nil {
		return nil, fmt.Errorf("unable to get remote feed: %v", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := fc.write(data); err != nil {
		return nil, err
	}

	feed, err := fc.parseRSSFeed(bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("unable to parse remote feed: %v", err)
	}
	return feed, nil
}

func (fc *FeedConfig) parseLocalFeed() (*rss.Feed, error) {
	b, err := os.ReadFile(fc.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &rss.Feed{}, nil
		}
		return nil, fmt.Errorf("unable to read local feed: %v", err)
	}

	feed, err := fc.parseRSSFeed(bytes.NewBuffer(b))
	if err != nil {
		return nil, fmt.Errorf("unable to parse local feed: %v", err)
	}

	return feed, nil
}

func (fc *FeedConfig) write(b []byte) error {
	f, err := os.Create(fc.path)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	return nil
}

func (fc *FeedConfig) parseRSSFeed(r io.Reader) (*rss.Feed, error) {
	fp := rss.Parser{}

	feed, err := fp.Parse(r)
	if err != nil {
		return nil, err
	}
	return feed, nil
}

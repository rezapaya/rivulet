package main

import (
	"bytes"
	"encoding/gob"
)

import (
	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"appengine/user"
)

type Pref struct {
	Field string
	Value string
	Score int64
}

type UserData struct {
	String   string
	TotalRead int64
	Bytes    []byte    `datastore:",noindex"`
	Feeds    []string  `datastore:"-"`
	Articles []Article `datastore:"-"`
	Prefs    []Pref    `datastore:"-"`
}

func (UserData) Template() string { return "user.html" }
func (UserData) Redirect() string { return "" }
func (UserData) Send() bool       { return true }

func (userdata *UserData) Load(c <-chan datastore.Property) (err error) {
	for p := range c {
		switch p.Name {
		case "String":
			userdata.String = p.Value.(string)
		case "TotalRead":
			userdata.TotalRead = p.Value.(int64)
		case "Bytes":
			reader := bytes.NewBuffer(p.Value.([]byte))
			decoder := gob.NewDecoder(reader)
			err = decoder.Decode(userdata)
			if err != nil {
				return
			}
		}
	}
	return
}

func (userdata *UserData) Save(c chan<- datastore.Property) (err error) {
	defer close(c)
	c <- datastore.Property{
		Name:  "String",
		Value: userdata.String,
	}
	c <- datastore.Property{
		Name:  "TotalRead",
		Value: userdata.TotalRead,
	}
	writer := bytes.Buffer{}
	encoder := gob.NewEncoder(&writer)
	err = encoder.Encode(userdata)
	if err != nil {
		return
	}
	c <- datastore.Property{Name: "Bytes", Value: writer.Bytes(), NoIndex: true}
	return
}

func newUserData(context appengine.Context, id string) (key *datastore.Key, userdata UserData, err error) {
	userdata.String = id
	userdata.TotalRead = 0

	for _, feed := range builtinFeeds {
		if feed.Default {
			err = subscribe(context, &userdata, feed.URL, feed.Default)
			if err != nil {
				printError(context, err, feed.URL)
				err = nil
				continue
			}
		}
	}

	if id != "default" {
		var defaultUser UserData
		_, defaultUser, err = mustGetUserData(context, "default")
		userdata.Articles = defaultUser.Articles
	}

	key, err = putUserData(context, datastore.NewIncompleteKey(context, "UserData", nil), userdata)

	return
}

func getUserData(context appengine.Context, id string) (key *datastore.Key, userdata UserData, err error) {
	key, err = GetFirst(context, "UserData", "String", id, &userdata)
	return
}

func mustGetUserData(context appengine.Context, id string) (key *datastore.Key, userdata UserData, err error) {
	key, userdata, err = getUserData(context, id)
	if err == datastore.Done {
		return newUserData(context, id)
	}
	return
}

func putUserData(context appengine.Context, oldkey *datastore.Key, userdata UserData) (newkey *datastore.Key, err error) {
	newkey, err = datastore.Put(context, oldkey, &userdata)
	return
}

func unsubscribe(context appengine.Context, userdata *UserData, url string) (err error) {
	for i, feed := range userdata.Feeds {
		if feed == url {
			userdata.Feeds = userdata.Feeds[:i+copy(userdata.Feeds[i:], userdata.Feeds[i+1:])]
			if err != nil {
				err = nil
				continue
			}
			break
		}
	}

	temp := make([]Article, 0, len(userdata.Articles))
	for _, article := range userdata.Articles {
		if article.FeedURL != url {
			temp = append(temp, article)
		}
	}
	userdata.Articles = temp

	var feed Feed
	var key *datastore.Key
	key, err = GetFirst(context, "Feed", "URL", url, &feed)
	if err != nil {
		return
	}

	for i, subscriber := range feed.Subscribers {
		if subscriber == userdata.String {
			feed.Subscribers = feed.Subscribers[:i+copy(feed.Subscribers[i:], feed.Subscribers[i+1:])]
			_, err = datastore.Put(context, key, &feed)
			break
		}
	}

	return
}

func unsubscribeUser(context appengine.Context, user *user.User, url string) (err error) {
	var userdata UserData
	var userkey *datastore.Key
	userkey, userdata, err = mustGetUserData(context, user.String())
	if err != nil {
		return
	}

	err = unsubscribe(context, &userdata, url)
	if err != nil {
		return
	}

	_, err = putUserData(context, userkey, userdata)

	return
}

func subscribe(context appengine.Context, userdata *UserData, url string, isdefault bool) (err error) {
	query := datastore.NewQuery("Feed").Filter("URL=", url)
	iterator := query.Run(context)

	feedsubscribed := false
	var feed Feed
	var key *datastore.Key
	key, err = iterator.Next(&feed)
	if err == datastore.Done {
		feed.URL = url
		feed.Subscribers = []string{userdata.String}
		feed.Default = isdefault
		key, err = datastore.Put(context, datastore.NewIncompleteKey(context, "Feed", nil), &feed)
		refreshSubscriptionURLDelay.Call(context, feed.URL)
		feedsubscribed = true
	}

	if !ContainsString(userdata.Feeds, url) {
		userdata.Feeds = append(userdata.Feeds, url)
		if !feedsubscribed {
			feed.Subscribers = append(feed.Subscribers, userdata.String)
			_, err = datastore.Put(context, key, &feed)
		}
	}

	return
}

func subscribeUser(context appengine.Context, user *user.User, url string) (err error) {
	var userdata UserData
	var userkey *datastore.Key
	userkey, userdata, err = mustGetUserData(context, user.String())
	if err != nil {
		return
	}

	err = subscribe(context, &userdata, url, false)
	if err != nil {
		return
	}

	_, err = putUserData(context, userkey, userdata)

	return
}

func getUserFeedList(context appengine.Context, user string) (feeds []FeedCache, err error) {
	var userdata UserData
	_, userdata, err = mustGetUserData(context, user)
	if err != nil {
		return
	}

	for _, feed := range userdata.Feeds {
		var item FeedCache
		_, err = memcache.Gob.Get(context, feed, &item)
		if err != nil {
			printError(context, err, feed)
			err = nil
			continue
		}
		feeds = append(feeds, item)
	}

	return
}

func getSuggestedFeeds(context appengine.Context, userdata UserData) (suggestedFeeds []Feed, err error) {
	for _, feed := range builtinFeeds {
		if !ContainsFeed(suggestedFeeds, feed.URL) && !ContainsString(userdata.Feeds, feed.URL) {
			suggestedFeeds = append(suggestedFeeds, feed)
		}
	}

	query := datastore.NewQuery("Feed")
	for iterator := query.Run(context); ; {
		var feed Feed
		_, err = iterator.Next(&feed)
		if err == datastore.Done {
			err = nil
			break
		} else if err != nil {
			printError(context, err, feed.URL)
			err = nil
			continue
		}
		if !ContainsFeed(suggestedFeeds, feed.URL) && !ContainsString(userdata.Feeds, feed.URL) {
			suggestedFeeds = append(suggestedFeeds, feed)
		}
	}

	return
}

func selected(context appengine.Context, userdata UserData, article Article) (UserData, error) {
	found := false
	for i, value := range userdata.Prefs {
		if value.Field == "field" && value.Value == article.FeedURL {
			found = true
			value.Score += 1
			userdata.Prefs[i] = value
			break
		}
	}

	if !found {
		userdata.Prefs = append(userdata.Prefs, Pref{
			Field: "feedurl",
			Value: article.FeedURL,
			Score: 1,
		})
	}

	return userdata, nil
}

func getRank(article []Pref, user []Pref) (score int64) {
	for _, userPref := range user {
		for _, articlePref := range article {
			if userPref.Field == articlePref.Field && userPref.Value == articlePref.Value {
				score += articlePref.Score * userPref.Score
			}
		}
	}
	return
}

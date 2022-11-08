package cron

import (
	"context"
	"github.com/pkg/errors"
	"github.com/viant/smirror/base"
	cfg "github.com/viant/smirror/config"
	"github.com/viant/smirror/cron/config"
	"github.com/viant/smirror/cron/meta"
	"github.com/viant/smirror/proxy"
	"github.com/viant/smirror/secret"
	"github.com/viant/afs"
	"github.com/viant/afs/file"
	"github.com/viant/afs/matcher"
	"github.com/viant/afs/storage"
	"github.com/viant/afs/url"
	"path"
	"sync"
	"time"
)

const Limit = 50

//Service represents a cron service
type Service interface {
	Tick(ctx context.Context) *Response
}

type service struct {
	config      *Config
	fs          afs.Service
	proxy       proxy.Service
	secret      secret.Service
	metaService meta.Service
}

//Tick run cron service
func (s *service) Tick(ctx context.Context) *Response {
	response := NewResponse(proxy.NewResponse())
	err := s.tick(ctx, response)
	if err != nil {
		response.Status = base.StatusError
		response.Error = err.Error()
	}
	return response
}

func (s *service) tick(ctx context.Context, response *Response) error {
	changed, err := s.config.Resources.ReloadIfNeeded(ctx, s.fs)
	if changed && err == nil {
		err = s.UpdateSecrets(ctx)
	}
	if err != nil {
		return err
	}
	var matched = make([]storage.Object, 0)
	for _, resource := range s.config.Resources.Rules {
		processed, err := s.processResource(ctx, resource, response)
		if err != nil {
			return err
		}
		if len(processed) > 0 {
			matched = append(matched, processed...)
			matched := &Matched{
				Resource: resource,
				URLs:     make([]string, 0),
			}
			matched.Add(processed...)
			response.Matched = append(response.Matched, matched)
		}
	}
	return err
}

func (s *service) processResource(ctx context.Context, resource *config.Rule, response *Response) ([]storage.Object, error) {
	objects, err := s.getResourceCandidates(ctx, resource)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get resource candidate %v", resource.Source.URL)
	}
	pending, err := s.metaService.PendingResources(ctx, objects)
	if err != nil || len(pending) == 0 {
		if err != nil {
			err = errors.Wrapf(err, "failed to read pending resource %v", len(objects))
		}
		return nil, err
	}
	if err = s.notifyAll(ctx, resource, pending, response); err != nil {
		return nil, errors.Wrapf(err, "failed to notify all")
	}
	err = s.metaService.AddProcessed(ctx, pending)
	if err != nil {
		err = errors.Wrapf(err, "failed to update processed")
	}
	return pending, err
}

func (s *service) notify(ctx context.Context, rule *config.Rule, object storage.Object, response *Response) error {
	proxyResponse := s.proxy.Proxy(ctx, &proxy.Request{
		Source: rule.Source.CloneWithURL(object.URL()),
		Dest:   &rule.Dest,
		Move:   rule.Move,
		Stream: true,
	})
	if proxyResponse.Error != "" {
		return errors.New(proxyResponse.Error)
	}
	for k, v := range proxyResponse.Moved {
		response.AddMoved(k, v)
	}
	for k, v := range proxyResponse.Copied {
		response.AddCopied(k, v)
	}
	for k, v := range proxyResponse.Invoked {
		response.AddInvoked(k, v)
	}
	return nil
}

func (s *service) notifyAll(ctx context.Context, resource *config.Rule, objects []storage.Object, response *Response) error {
	if len(objects) == 0 {
		return nil
	}
	queue := make(chan storage.Object, len(objects))
	waitGroup := &sync.WaitGroup{}
	var errorChannel = make(chan error, len(objects))
	for worker := 0; worker < Limit; worker++ {
		waitGroup.Add(1)

		go func() {
			defer waitGroup.Done()

			for object := range queue {
				errorChannel <- s.notify(ctx, resource, object, response) // blocking wait for work
			}
		}()
	}
	for i := range objects {
		// log.Printf("Work %s enqueued\n", objects[i])
		queue <- objects[i]
	}
	close(queue)
	waitGroup.Wait()
	for i := 0; i < len(objects); i++ {
		if err := <-errorChannel; err != nil {
			return err
		}
	}
	return nil
}

func (s *service) getResourceCandidates(ctx context.Context, resource *config.Rule) ([]storage.Object, error) {
	var result = make([]storage.Object, 0)
	options, err := s.secret.StorageOpts(ctx, &resource.Source)
	if err != nil {
		return nil, err
	}
	options = s.addLastModifiedTimeMatcher(options)
	return result, s.appendResources(ctx, resource.Source.URL, &result, &resource.Source.Basic, options)
}

func (s *service) appendResources(ctx context.Context, URL string, result *[]storage.Object, filter *matcher.Basic, options []storage.Option) error {
	objects, err := s.fs.List(ctx, URL, options...)
	if err != nil {
		return err
	}
	for i := range objects {
		if i == 0 && objects[i].IsDir() {
			continue
		}
		if objects[i].IsDir() {
			if err = s.appendResources(ctx, objects[i].URL(), result, filter, options); err != nil {
				return err
			}
			continue
		}
		_, URLPath := url.Base(objects[i].URL(), file.Scheme)
		parent, _ := path.Split(URLPath)
		if filter.Match(parent, objects[i]) {
			*result = append(*result, objects[i])
		}
	}
	return nil
}

func (s *service) addLastModifiedTimeMatcher(options []storage.Option) []storage.Option {
	afterTime := time.Now().Add(-s.config.TimeWindow.Duration)
	return append(options, matcher.NewModification(nil, &afterTime))
}

func (s *service) Init(ctx context.Context, fs afs.Service) error {
	if s.config.SourceScheme == "" {
		s.config.SourceScheme = url.Scheme(s.config.MetaURL, "")
	}
	var err error
	cfg, _ := proxy.NewConfig(ctx)
	s.proxy = proxy.New(s.fs, cfg, s.secret)
	if err = s.config.Init(ctx, fs); err == nil {
		err = s.UpdateSecrets(ctx)
	}
	return err
}

func (s *service) UpdateSecrets(ctx context.Context) error {
	if s.secret == nil {
		return nil
	}
	resources := make([]*cfg.Resource, 0)
	for i := range s.config.Resources.Rules {
		resources = append(resources, &s.config.Resources.Rules[i].Source)
		resources = append(resources, &s.config.Resources.Rules[i].Dest)
	}
	return s.secret.Init(ctx, s.fs, resources)
}

//New returns new cron service
func New(ctx context.Context, config *Config, fs afs.Service) (Service, error) {
	err := config.Init(ctx, fs)
	if err != nil {
		return nil, err
	}
	meteService := meta.New(config.MetaURL, config.TimeWindow.Duration*2, fs)
	result := &service{
		config:      config,
		fs:          fs,
		secret:      secret.New(config.SourceScheme, fs),
		metaService: meteService,
	}

	return result, result.Init(ctx, fs)
}

package cron

import (
	"context"
	"fmt"
	"github.com/viant/afs"
	"github.com/viant/afs/matcher"
	"github.com/viant/afs/storage"
	"github.com/viant/afs/url"
	"github.com/viant/afsc/s3"
	"smirror/base"
	cfg "smirror/config"
	"smirror/cron/config"
	"smirror/cron/meta"
	"smirror/cron/trigger"
	"smirror/cron/trigger/lambda"
	"smirror/cron/trigger/mem"
	"smirror/secret"
	"strings"
	"sync"
	"time"
)

//Service represents a cron service
type Service interface {
	Tick(ctx context.Context) error
}

type service struct {
	config      *Config
	fs          afs.Service
	dest        trigger.Service
	secret      secret.Service
	metaService meta.Service
}

//Tick run cron service
func (s *service) Tick(ctx context.Context) error {
	matched, err := s.tick(ctx)
	s.reportMatched(matched, err)
	return err
}

func (s *service) tick(ctx context.Context) ([]storage.Object, error) {
	changed, err := s.config.Resources.ReloadIfNeeded(ctx, s.fs)
	if changed && err == nil {
		err = s.UpdateSecrets(ctx)
	}
	if err != nil {
		return nil, err
	}
	var matched = make([]storage.Object, 0)
	for _, resource := range s.config.Resources.Rules {
		processed, err := s.processResource(ctx, resource)
		if err != nil {
			return matched, err
		}
		if len(processed) > 0 {
			matched = append(matched, processed...)
		}
	}
	return matched, err
}

func (s *service) reportMatched(matched []storage.Object, err error) {
	if ! base.IsLoggingEnabled() {
		return
	}
	var URLs = make([]string, 0)
	for i := range matched {
		URLs = append(URLs, fmt.Sprintf(`"%v"`, matched[i].URL()))
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	message := fmt.Sprintf(`{"status":"%v", matched":[%v]}`, status, strings.Join(URLs, ", "))
	fmt.Println(message)
}

func (s *service) processResource(ctx context.Context, resource *config.Resource) ([]storage.Object, error) {
	objects, err := s.getResourceCandidates(ctx, resource)
	if err != nil {
		return nil, err
	}
	pendings, err := s.metaService.PendingResources(ctx, objects)
	if err != nil || len(pendings) == 0 {
		return nil, err
	}

	if err = s.notifyAll(ctx, resource, pendings); err != nil {
		return nil, err
	}
	return pendings, s.metaService.AddProcessed(ctx, pendings)
}

func (s *service) notify(ctx context.Context, resource *config.Resource, object storage.Object) error {
	return s.dest.Trigger(ctx, resource, object)
}

func (s *service) notifyAll(ctx context.Context, resource *config.Resource, objects []storage.Object) error {
	if len(objects) == 0 {
		return nil
	}
	waitGroup := &sync.WaitGroup{}
	waitGroup.Add(len(objects))
	var errors = make(chan error, len(objects))
	for i := range objects {
		go func(object storage.Object) {
			defer waitGroup.Done()
			errors <- s.notify(ctx, resource, object)
		}(objects[i])
	}
	for i := 0; i < len(objects); i++ {
		if err := <-errors; err != nil {
			return err
		}
	}
	return nil
}

func (s *service) getResourceCandidates(ctx context.Context, resource *config.Resource) ([]storage.Object, error) {
	var result = make([]storage.Object, 0)
	options, err := s.secret.StorageOpts(ctx, &resource.Resource)
	if err != nil {
		return nil, err
	}
	return result, s.appendResources(ctx, resource.URL, &result, options)
}

func (s *service) appendResources(ctx context.Context, URL string, result *[]storage.Object, options []storage.Option) error {
	objects, err := s.fs.List(ctx, URL, options...)
	if err != nil {
		return err
	}
	for i := range objects {
		if i == 0 && objects[i].IsDir() {
			continue
		}
		if objects[i].IsDir() {
			if err = s.appendResources(ctx, objects[i].URL(), result, options); err != nil {
				return err
			}
			continue
		}
		*result = append(*result, objects[i])
	}
	return nil
}

func (s *service) addLastModifiedTimeMatcher(options []storage.Option) []storage.Option {
	afterTime := time.Now().Add(-s.config.TimeWindow.Duration)
	return append(options, matcher.NewModification(nil, &afterTime))
}

func (s *service) Init(ctx context.Context, fs afs.Service) error {
	var err error
	if s.config.SourceScheme == "" {
		s.config.SourceScheme = url.Scheme(s.config.MetaURL, "")
	}
	switch s.config.SourceScheme {
	case s3.Scheme:
		s.dest, err = lambda.New()
	case mem.Scheme: //testing only
		s.dest = mem.New(fs)
	default:
		err = fmt.Errorf("unsupported source scheme: %v", s.config.SourceScheme)
	}
	if err != nil {
		return err
	}
	if err = s.config.Init(ctx, fs); err == nil {
		err = s.UpdateSecrets(ctx)
	}
	return err
}

func (s *service) UpdateSecrets(ctx context.Context) error {
	if s.secret == nil {
		return nil
	}
	resources := make([]*cfg.Resource, len(s.config.Resources.Rules))
	for i := range s.config.Resources.Rules {
		resources[i] = &s.config.Resources.Rules[i].Resource
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
		secret:      secret.New(config.SourceScheme),
		metaService: meteService,
	}

	return result, result.Init(ctx, fs)
}
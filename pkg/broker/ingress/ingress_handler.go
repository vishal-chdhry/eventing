/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ingress

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	opencensusclient "github.com/cloudevents/sdk-go/observability/opencensus/v2/client"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/client"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"

	"knative.dev/pkg/apis"
	"knative.dev/pkg/network"

	"knative.dev/eventing/pkg/apis/eventing"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	"knative.dev/eventing/pkg/broker"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1"
	"knative.dev/eventing/pkg/kncloudevents"
	"knative.dev/eventing/pkg/tracing"
	"knative.dev/eventing/pkg/utils"
	duckv1 "knative.dev/pkg/apis/duck/v1"
)

const (
	// noDuration signals that the dispatch step hasn't started
	noDuration                       = -1
	defaultMaxIdleConnections        = 1000
	defaultMaxIdleConnectionsPerHost = 1000
)

type Handler struct {
	// Defaults sets default values to incoming events
	Defaulter client.EventDefaulter
	// Reporter reports stats of status code and dispatch time
	Reporter StatsReporter
	// BrokerLister gets broker objects
	BrokerLister eventinglisters.BrokerLister

	Logger *zap.Logger
}

func NewHandler(logger *zap.Logger, reporter StatsReporter, defaulter client.EventDefaulter, brokerLister eventinglisters.BrokerLister) (*Handler, error) {
	connectionArgs := kncloudevents.ConnectionArgs{
		MaxIdleConns:        defaultMaxIdleConnections,
		MaxIdleConnsPerHost: defaultMaxIdleConnectionsPerHost,
	}
	kncloudevents.ConfigureConnectionArgs(&connectionArgs)

	return &Handler{
		Defaulter:    defaulter,
		Reporter:     reporter,
		Logger:       logger,
		BrokerLister: brokerLister,
	}, nil
}

func (h *Handler) getBroker(name, namespace string) (*eventingv1.Broker, error) {
	broker, err := h.BrokerLister.Brokers(namespace).Get(name)
	if err != nil {
		h.Logger.Warn("Broker getter failed")
		return nil, err
	}
	return broker, nil
}

func (h *Handler) guessChannelAddress(name, namespace, domain string) (*duckv1.Addressable, error) {
	broker, err := h.getBroker(name, namespace)
	if err != nil {
		return nil, err
	}

	return &duckv1.Addressable{
		URL: &apis.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s-kne-trigger-kn-channel.%s.svc.%s", name, namespace, domain),
			Path:   "/",
		},
		CACerts: broker.Status.Address.CACerts,
	}, nil
}

func (h *Handler) getChannelAddress(name, namespace string) (*duckv1.Addressable, error) {
	broker, err := h.getBroker(name, namespace)
	if err != nil {
		return nil, err
	}
	if broker.Status.Annotations == nil {
		return nil, fmt.Errorf("broker status annotations uninitialized")
	}
	address, present := broker.Status.Annotations[eventing.BrokerChannelAddressStatusAnnotationKey]
	if !present {
		return nil, fmt.Errorf("channel address not found in broker status annotations")
	}

	url, err := apis.ParseURL(address)
	if err != nil {
		return nil, fmt.Errorf("failed to parse channel address url")
	}

	addr := &duckv1.Addressable{
		URL:     url,
		CACerts: broker.Status.Address.CACerts,
	}
	return addr, nil
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Allow", "POST, OPTIONS")
	// validate request method
	if request.Method == http.MethodOptions {
		writer.Header().Set("WebHook-Allowed-Origin", "*") // Accept from any Origin:
		writer.Header().Set("WebHook-Allowed-Rate", "*")   // Unlimited requests/minute
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method != http.MethodPost {
		h.Logger.Warn("unexpected request method", zap.String("method", request.Method))
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// validate request URI
	if request.RequestURI == "/" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	nsBrokerName := strings.Split(strings.TrimSuffix(request.RequestURI, "/"), "/")
	if len(nsBrokerName) != 3 {
		h.Logger.Info("Malformed uri", zap.String("URI", request.RequestURI))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := request.Context()

	message := cehttp.NewMessageFromHttpRequest(request)
	defer message.Finish(nil)

	event, err := binding.ToEvent(ctx, message)
	if err != nil {
		h.Logger.Warn("failed to extract event from request", zap.Error(err))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	// run validation for the extracted event
	validationErr := event.Validate()
	if validationErr != nil {
		h.Logger.Warn("failed to validate extracted event", zap.Error(validationErr))
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	brokerNamespace := nsBrokerName[1]
	brokerName := nsBrokerName[2]
	brokerNamespacedName := types.NamespacedName{
		Name:      brokerName,
		Namespace: brokerNamespace,
	}

	ctx, span := trace.StartSpan(ctx, tracing.BrokerMessagingDestination(brokerNamespacedName))
	defer span.End()

	if span.IsRecordingEvents() {
		span.AddAttributes(
			tracing.MessagingSystemAttribute,
			tracing.MessagingProtocolHTTP,
			tracing.BrokerMessagingDestinationAttribute(brokerNamespacedName),
			tracing.MessagingMessageIDAttribute(event.ID()),
		)
		span.AddAttributes(opencensusclient.EventTraceAttributes(event)...)
	}

	reporterArgs := &ReportArgs{
		ns:        brokerNamespace,
		broker:    brokerName,
		eventType: event.Type(),
	}

	statusCode, dispatchTime := h.receive(ctx, request.Header, event, brokerNamespace, brokerName)
	if dispatchTime > noDuration {
		_ = h.Reporter.ReportEventDispatchTime(reporterArgs, statusCode, dispatchTime)
	}
	_ = h.Reporter.ReportEventCount(reporterArgs, statusCode)

	writer.WriteHeader(statusCode)
}

func (h *Handler) receive(ctx context.Context, headers http.Header, event *cloudevents.Event, brokerNamespace, brokerName string) (int, time.Duration) {

	// Setting the extension as a string as the CloudEvents sdk does not support non-string extensions.
	event.SetExtension(broker.EventArrivalTime, cloudevents.Timestamp{Time: time.Now()})
	if h.Defaulter != nil {
		newEvent := h.Defaulter(ctx, *event)
		event = &newEvent
	}

	if ttl, err := broker.GetTTL(event.Context); err != nil || ttl <= 0 {
		h.Logger.Debug("dropping event based on TTL status.", zap.Int32("TTL", ttl), zap.String("event.id", event.ID()), zap.Error(err))
		return http.StatusBadRequest, noDuration
	}

	channelAddress, err := h.getChannelAddress(brokerName, brokerNamespace)
	if err != nil {
		h.Logger.Warn("Failed to get channel address, falling back on guess", zap.Error(err))
		channelAddress, err = h.guessChannelAddress(brokerName, brokerNamespace, network.GetClusterDomainName())
		if err != nil {
			h.Logger.Warn("Broker not found in the namespace", zap.Error(err))
		}
	}

	return h.send(ctx, headers, event, *channelAddress)
}

func (h *Handler) send(ctx context.Context, headers http.Header, event *cloudevents.Event, target duckv1.Addressable) (int, time.Duration) {

	request, err := kncloudevents.NewCloudEventRequest(ctx, target)
	if err != nil {
		h.Logger.Error("failed to create event request.", zap.Error(err))
		return http.StatusInternalServerError, noDuration
	}

	message := binding.ToMessage(event)
	defer message.Finish(nil)

	additionalHeaders := utils.PassThroughHeaders(headers)
	err = kncloudevents.WriteRequestWithAdditionalHeaders(ctx, message, request, additionalHeaders)
	if err != nil {
		h.Logger.Error("failed to write request additionalHeaders.", zap.Error(err))
		return http.StatusInternalServerError, noDuration
	}

	resp, dispatchTime, err := h.sendAndRecordDispatchTime(request)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		h.Logger.Error("failed to dispatch event", zap.Error(err))
		return http.StatusInternalServerError, dispatchTime
	}

	return resp.StatusCode, dispatchTime
}

func (h *Handler) sendAndRecordDispatchTime(request *kncloudevents.CloudEventRequest) (*http.Response, time.Duration, error) {
	start := time.Now()
	resp, err := request.Send()
	dispatchTime := time.Since(start)
	return resp, dispatchTime, err
}

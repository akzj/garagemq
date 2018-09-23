package server

import (
	"errors"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/valinurovam/garagemq/amqp"
	"github.com/valinurovam/garagemq/binding"
	"github.com/valinurovam/garagemq/config"
	"github.com/valinurovam/garagemq/exchange"
	"github.com/valinurovam/garagemq/metrics"
	"github.com/valinurovam/garagemq/msgstorage"
	"github.com/valinurovam/garagemq/queue"
	"github.com/valinurovam/garagemq/srvstorage"
)

const exDefaultName = ""

// VirtualHost represents AMQP virtual host
// Each virtual host is "parent" for its queues and exchanges
type VirtualHost struct {
	name            string
	system          bool
	exLock          sync.Mutex
	exchanges       map[string]*exchange.Exchange
	quLock          sync.Mutex
	queues          map[string]*queue.Queue
	msgStorage      *msgstorage.MsgStorage
	srv             *Server
	srvStorage      *srvstorage.SrvStorage
	srvConfig       *config.Config
	logger          *log.Entry
	autoDeleteQueue chan string
}

// NewVhost returns instance of VirtualHost
// When instantiating virtual host we
// 1) init system exchanges
// 2) load durable exchanges, queues and bindings from server storage
// 3) load persisted messages from message store into all initiated queues
// 4) run confirm loop
// Only after that vhost is in state running
func NewVhost(name string, system bool, msgStorage *msgstorage.MsgStorage, srv *Server) *VirtualHost {
	vhost := &VirtualHost{
		name:            name,
		system:          system,
		exchanges:       make(map[string]*exchange.Exchange),
		queues:          make(map[string]*queue.Queue),
		msgStorage:      msgStorage,
		srvStorage:      srv.storage,
		srvConfig:       srv.config,
		srv:             srv,
		autoDeleteQueue: make(chan string, 1),
	}

	vhost.logger = log.WithFields(log.Fields{
		"vhost": name,
	})

	vhost.initSystemExchanges()
	vhost.loadExchanges()
	vhost.loadQueues()
	vhost.loadBindings()

	vhost.logger.Info("Load messages into queues")

	vhost.loadMessagesIntoQueues()
	for _, q := range vhost.GetQueues() {
		q.Start()
		vhost.logger.WithFields(log.Fields{
			"name":   q.GetName(),
			"length": q.Length(),
		}).Info("Messages loaded into queue")
	}

	go vhost.handleConfirms()
	go vhost.handleAutoDeleteQueue()

	return vhost
}

func (vhost *VirtualHost) handleAutoDeleteQueue() {
	for queueName := range vhost.autoDeleteQueue {
		vhost.DeleteQueue(queueName, false, false)
	}
}

func (vhost *VirtualHost) handleConfirms() {
	confirmsChan := vhost.msgStorage.ReceiveConfirms()
	for confirm := range confirmsChan {
		if !confirm.ConfirmMeta.CanConfirm() {
			continue
		}
		channel := vhost.srv.getConfirmChannel(confirm.ConfirmMeta)
		if channel == nil {
			continue
		}
		channel.addConfirm(confirm.ConfirmMeta)
	}
}

func (vhost *VirtualHost) initSystemExchanges() {
	vhost.logger.Info("Initialize host default exchanges...")
	for _, exType := range []byte{
		exchange.ExTypeDirect,
		exchange.ExTypeFanout,
		exchange.ExTypeHeaders,
		exchange.ExTypeTopic,
	} {
		exTypeAlias, _ := exchange.GetExchangeTypeAlias(exType)
		exName := "amq." + exTypeAlias
		vhost.AppendExchange(exchange.NewExchange(exName, exType, true, false, false, true))
	}

	systemExchange := exchange.NewExchange(exDefaultName, exchange.ExTypeDirect, true, false, false, true)
	vhost.AppendExchange(systemExchange)
}

// GetQueue returns queue by name or nil if not exists
func (vhost *VirtualHost) GetQueue(name string) *queue.Queue {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	return vhost.getQueue(name)
}

// GetQueues return all vhost's queues
func (vhost *VirtualHost) GetQueues() map[string]*queue.Queue {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	return vhost.queues
}

func (vhost *VirtualHost) getQueue(name string) *queue.Queue {
	return vhost.queues[name]
}

// GetExchange returns exchange by name or nil if not exists
func (vhost *VirtualHost) GetExchange(name string) *exchange.Exchange {
	vhost.exLock.Lock()
	defer vhost.exLock.Unlock()
	return vhost.getExchange(name)
}

func (vhost *VirtualHost) getExchange(name string) *exchange.Exchange {
	return vhost.exchanges[name]
}

func (vhost *VirtualHost) GetExchanges() map[string]*exchange.Exchange {
	return vhost.exchanges
}

// GetDefaultExchange returns default exchange
func (vhost *VirtualHost) GetDefaultExchange() *exchange.Exchange {
	return vhost.exchanges[exDefaultName]
}

// AppendExchange append new exchange and persist if it is durable
func (vhost *VirtualHost) AppendExchange(ex *exchange.Exchange) {
	vhost.exLock.Lock()
	defer vhost.exLock.Unlock()
	exTypeAlias, _ := exchange.GetExchangeTypeAlias(ex.ExType())
	vhost.logger.WithFields(log.Fields{
		"name": ex.GetName(),
		"type": exTypeAlias,
	}).Info("Append exchange")
	vhost.exchanges[ex.GetName()] = ex

	if ex.IsDurable() && !ex.IsSystem() {
		vhost.srvStorage.AddExchange(vhost.name, ex)
	}

	ex.SetMetrics(&exchange.MetricsState{
		MsgIn:  metrics.AddCounter(fmt.Sprintf("exchange.%s.%s.msg_in", vhost.name, ex.GetName())),
		MsgOut: metrics.AddCounter(fmt.Sprintf("exchange.%s.%s.msg_out", vhost.name, ex.GetName())),
	})

}

// NewQueue returns new instance of queue by params
// we can't use just queue.NewQueue, cause we need to set msgStorage to queue
func (vhost *VirtualHost) NewQueue(name string, connID uint64, exclusive bool, autoDelete bool, durable bool, shardSize int) *queue.Queue {
	return queue.NewQueue(
		name,
		connID,
		exclusive,
		autoDelete,
		durable,
		shardSize,
		vhost.msgStorage,
		vhost.autoDeleteQueue,
	)
}

// AppendQueue append new queue and persist if it is durable and
// bindings into default exchange
func (vhost *VirtualHost) AppendQueue(qu *queue.Queue) {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()
	vhost.logger.WithFields(log.Fields{
		"queueName": qu.GetName(),
	}).Info("Append queue")

	vhost.queues[qu.GetName()] = qu

	ex := vhost.GetDefaultExchange()
	bind := binding.NewBinding(qu.GetName(), exDefaultName, qu.GetName(), &amqp.Table{}, false)
	ex.AppendBinding(bind)

	if qu.IsDurable() {
		vhost.srvStorage.AddQueue(vhost.name, qu)
	}

	qu.SetMetrics(&queue.MetricsState{
		Ready:    metrics.AddCounter(fmt.Sprintf("queue.%s.%s.ready", vhost.name, qu.GetName())),
		Unacked:  metrics.AddCounter(fmt.Sprintf("queue.%s.%s.unacked", vhost.name, qu.GetName())),
		Total:    metrics.AddCounter(fmt.Sprintf("queue.%s.%s.total", vhost.name, qu.GetName())),
		Incoming: metrics.AddCounter(fmt.Sprintf("queue.%s.%s.incoming", vhost.name, qu.GetName())),
		Deliver:  metrics.AddCounter(fmt.Sprintf("queue.%s.%s.deliver", vhost.name, qu.GetName())),
		Get:      metrics.AddCounter(fmt.Sprintf("queue.%s.%s.get", vhost.name, qu.GetName())),
		Ack:      metrics.AddCounter(fmt.Sprintf("queue.%s.%s.ack", vhost.name, qu.GetName())),

		ServerReady:   vhost.srv.metrics.Ready,
		ServerUnacked: vhost.srv.metrics.Unacked,
		ServerTotal:   vhost.srv.metrics.Total,
		ServerDeliver: vhost.srv.metrics.Deliver,
		ServerAck:     vhost.srv.metrics.Ack,
	})
}

// PersistBinding store binding into server storage
func (vhost *VirtualHost) PersistBinding(binding *binding.Binding) {
	vhost.srvStorage.AddBinding(vhost.name, binding)
}

// RemoveBindings remove given bindings from server storage
func (vhost *VirtualHost) RemoveBindings(bindings []*binding.Binding) {
	for _, bind := range bindings {
		vhost.srvStorage.DelBinding(vhost.name, bind)
	}
}

func (vhost *VirtualHost) loadQueues() {
	vhost.logger.Info("Initialize queues...")
	queues := vhost.srvStorage.GetVhostQueues(vhost.name)
	if len(queues) == 0 {
		return
	}
	for _, q := range queues {
		vhost.AppendQueue(
			vhost.NewQueue(q.GetName(), 0, false, q.IsAutoDelete(), q.IsDurable(), vhost.srvConfig.Queue.ShardSize),
		)
	}
}

func (vhost *VirtualHost) loadMessagesIntoQueues() {
	var wg sync.WaitGroup
	for queueName, q := range vhost.queues {
		wg.Add(1)
		go func(queueName string, queue *queue.Queue) {
			vhost.msgStorage.IterateByQueue(queueName, func(message *amqp.Message) {
				queue.Push(message, true)
			})
			wg.Done()
		}(queueName, q)
	}
	wg.Wait()
}

func (vhost *VirtualHost) loadExchanges() {
	vhost.logger.Info("Initialize exchanges...")
	exchanges := vhost.srvStorage.GetVhostExchanges(vhost.name)
	if len(exchanges) == 0 {
		return
	}
	for _, ex := range exchanges {
		vhost.AppendExchange(ex)
	}
}

func (vhost *VirtualHost) loadBindings() {
	vhost.logger.Info("Initialize bindings...")
	bindings := vhost.srvStorage.GetVhostBindings(vhost.name)
	if len(bindings) == 0 {
		return
	}
	for _, bind := range bindings {
		ex := vhost.getExchange(bind.Exchange)
		if ex != nil {
			ex.AppendBinding(bind)
		}
	}
}

// DeleteQueue delete queue from virtual host and all bindings to that queue
// Also queue will be removed from server storage
func (vhost *VirtualHost) DeleteQueue(queueName string, ifUnused bool, ifEmpty bool) (uint64, error) {
	vhost.quLock.Lock()
	defer vhost.quLock.Unlock()

	qu := vhost.getQueue(queueName)
	if qu == nil {
		return 0, errors.New("not found")
	}

	var length, err = qu.Delete(ifUnused, ifEmpty)
	if err != nil {
		return 0, err
	}
	for _, ex := range vhost.exchanges {
		removedBindings := ex.RemoveQueueBindings(queueName)
		vhost.RemoveBindings(removedBindings)
	}
	vhost.srvStorage.DelQueue(vhost.name, qu)
	delete(vhost.queues, queueName)

	return length, nil
}

// Stop properly stop virtual host
// TODO: properly stop confirm loop
func (vhost *VirtualHost) Stop() error {
	vhost.quLock.Lock()
	vhost.exLock.Lock()
	defer vhost.quLock.Unlock()
	defer vhost.exLock.Unlock()
	vhost.logger.Info("Stop virtual host")
	for _, qu := range vhost.queues {
		qu.Stop()
		vhost.logger.WithFields(log.Fields{
			"queueName": qu.GetName(),
		}).Info("Queue stopped")
	}

	vhost.msgStorage.Close()
	vhost.logger.Info("Storage closed")
	close(vhost.autoDeleteQueue)
	return nil
}

func (vhost *VirtualHost) GetName() string {
	return vhost.name
}

package control

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"

	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/service/context"
	"github.com/pydio/cells/common/sync/merger"
	"github.com/pydio/cells/common/sync/model"
	"github.com/pydio/cells/common/sync/task"
	"github.com/pydio/sync/common"
	"github.com/pydio/sync/config"
	"github.com/pydio/sync/endpoint"
)

type Syncer struct {
	task    *task.Sync
	stop    chan bool
	uuid    string
	watches bool

	eventsChan  chan interface{}
	patchStatus chan merger.ProcessStatus
	patchDone   chan interface{}
	cmd         *model.Command

	serviceCtx context.Context
	stateStore StateStore
	patchStore *endpoint.PatchStore
	taskPaused bool
	// To be stored in state store?
	lastFailedPatch merger.Patch
}

func NewSyncer(conf *config.Task) (*Syncer, error) {
	parseMessage := "invalid arguments: please provide left and right endpoints using a valid URI"
	if conf.LeftURI == "" || conf.RightURI == "" {
		return nil, fmt.Errorf(parseMessage)
	}
	leftEndpoint, err := endpoint.EndpointFromURI(conf.LeftURI, conf.RightURI)
	if err != nil {
		return nil, errors.Wrap(err, parseMessage)
	}
	rightEndpoint, err := endpoint.EndpointFromURI(conf.RightURI, conf.LeftURI)
	if err != nil {
		return nil, errors.Wrap(err, parseMessage)
	}

	var direction model.DirectionType
	switch conf.Direction {
	case "Bi":
		direction = model.DirectionBi
	case "Left":
		direction = model.DirectionLeft
	case "Right":
		direction = model.DirectionRight
	default:
		return nil, fmt.Errorf("unsupported direction type, please use one of Bi, Left, Right")
	}

	ctx := servicecontext.WithServiceName(context.Background(), "sync-task")
	ctx = servicecontext.WithServiceColor(ctx, servicecontext.ServiceColorGrpc)

	taskUuid := conf.Uuid
	syncTask := task.NewSync(leftEndpoint, rightEndpoint, direction, conf.SelectiveRoots...)

	syncer := &Syncer{
		uuid:        taskUuid,
		serviceCtx:  ctx,
		task:        syncTask,
		watches:     conf.Realtime,
		stateStore:  NewMemoryStateStore(conf),
		stop:        make(chan bool, 1),
		eventsChan:  make(chan interface{}),
		patchStatus: make(chan merger.ProcessStatus),
		patchDone:   make(chan interface{}),
		cmd:         model.NewCommand(),
	}
	if patchStore, err := endpoint.NewPatchStore(taskUuid); err == nil {
		syncer.patchStore = patchStore
		syncTask.SetPatchPiper(syncer.patchStore)
	} else {
		log.Logger(ctx).Error("Cannot open patch store: " + err.Error())
	}
	var lastBatchSize int
	go func() {
		for {
			select {
			case l, ok := <-syncer.patchStatus:
				if !ok {
					return
				}
				msg := "Status: " + l.StatusString
				if l.Progress > 0 {
					msg += fmt.Sprintf(" - Progress: %d%%", int64(l.Progress*100))
				}
				var status common.SyncStatus
				if l.IsError {
					status = common.SyncStatusError
					log.Logger(ctx).Error(msg)
				} else {
					status = common.SyncStatusProcessing
					log.Logger(ctx).Debug(msg)
				}
				state := syncer.stateStore.UpdateProcessStatus(l, status)
				bus.Pub(state, TopicState)

			case data, ok := <-syncer.patchDone:
				if !ok {
					return
				}
				deferIdle := true
				if patch, ok := data.(merger.Patch); ok {
					stats := patch.Stats()
					if val, ok := stats["Errors"]; ok {
						errs := val.(map[string]int)
						msg := fmt.Sprintf("Processing ended on error (%d errors)! Pausing task.", errs["Total"])
						log.Logger(ctx).Error(msg)
						syncer.lastFailedPatch = patch
						state := syncer.stateStore.UpdateProcessStatus(merger.ProcessStatus{StatusString: msg, Progress: 1}, common.SyncStatusError)
						bus.Pub(state, TopicState)
						deferIdle = false
					} else if err, ok := patch.HasError(); ok {
						msg := fmt.Sprintf("Processing ended on error (%s)! Pausing task.", err.Error())
						log.Logger(ctx).Error(msg)
						syncer.lastFailedPatch = patch
						state := syncer.stateStore.UpdateProcessStatus(merger.ProcessStatus{StatusString: msg, Progress: 1}, common.SyncStatusError)
						bus.Pub(state, TopicState)
						deferIdle = false
					} else if val, ok := stats["Processed"]; ok {
						processed := val.(map[string]int)
						msg := fmt.Sprintf("Finished Processing %d files and folders", processed["Total"])
						log.Logger(ctx).Info(msg)
						state := syncer.stateStore.UpdateProcessStatus(merger.ProcessStatus{StatusString: msg, Progress: 1}, common.SyncStatusIdle)
						bus.Pub(state, TopicState)
					} else {
						state := syncer.stateStore.UpdateProcessStatus(merger.ProcessStatus{StatusString: "Task Idle"}, common.SyncStatusIdle)
						bus.Pub(state, TopicState)
						deferIdle = false
					}
					syncer.patchStore.Store(patch)
				}
				if deferIdle {
					go func() {
						<-time.After(3 * time.Second)
						state := syncer.stateStore.UpdateProcessStatus(merger.ProcessStatus{StatusString: "Task Idle"}, common.SyncStatusIdle)
						bus.Pub(state, TopicState)
					}()
				}

			case e := <-syncer.eventsChan:
				go GetBus().Pub(e, TopicSync_+taskUuid)

			case <-time.After(15 * time.Minute):
				if lastBatchSize > 0 {
					fmt.Println("Sending Loop after 15mn Idle Time")
					GetBus().Pub(MessageSyncLoop, TopicSync_+taskUuid)
				}
				break
			}
		}
	}()
	syncTask.SetupCmd(syncer.cmd)
	syncTask.SetupEventsChan(syncer.patchStatus, syncer.patchDone, syncer.eventsChan)
	return syncer, nil

}

func (s *Syncer) dispatch(ctx context.Context, done chan bool) {

	bus := GetBus()
	topic := bus.Sub(TopicSyncAll, TopicSync_+s.uuid)

	for {
		select {

		case <-done:

			bus.Unsub(topic)
			s.task.Shutdown()
			close(s.eventsChan)
			close(s.patchDone)
			close(s.patchStatus)
			s.cmd.Stop()
			s.patchStore.Stop()
			log.Logger(ctx).Info("Stopping Service")
			return

		case message := <-topic:

			switch message {
			case MessageRestart:
				// Message from supervisor, just update status
				bus.Pub(s.stateStore.UpdateSyncStatus(common.SyncStatusRestarting), TopicState)
			case MessageHalt:
				// Message from supervisor, just update status
				bus.Pub(s.stateStore.UpdateSyncStatus(common.SyncStatusStopping), TopicState)
			case MessageResync:
				// Trigger a full resync
				s.task.Run(ctx, false, true)
			case MessageResyncDry:
				// Trigger a dry-run
				s.task.Run(ctx, true, true)
			case MessageSyncLoop:
				// Trigger the loop
				if s.lastFailedPatch != nil {
					s.task.ReApplyPatch(ctx, s.lastFailedPatch)
					s.lastFailedPatch = nil
				} else {
					s.task.Run(ctx, false, false)
				}
			case MessagePublishState:
				// Broadcast current state
				bus.Pub(s.stateStore.LastState(), TopicState)
			case MessageInterrupt:
				s.cmd.Publish(model.Interrupt)
			case MessagePause:
				// Stop watching for events
				s.task.Pause(ctx)
				s.taskPaused = true
				state := s.stateStore.UpdateSyncStatus(common.SyncStatusPaused)
				bus.Pub(state, TopicState)
			case MessageResume:
				// Start watching for events
				s.task.Resume(ctx)
				s.taskPaused = false
				state := s.stateStore.UpdateSyncStatus(common.SyncStatusIdle)
				bus.Pub(state, TopicState)
				s.task.Run(ctx, false, false)
			case MessageDisable:
				// Disable Task
				s.task.Shutdown()
				state := s.stateStore.UpdateSyncStatus(common.SyncStatusDisabled)
				bus.Pub(state, TopicState)
			default:
				// Received info about an Endpoint
				if status, ok := message.(*model.EndpointStatus); ok {
					initialConnState := s.stateStore.BothConnected()
					var epConnected bool
					if status.WatchConnection == model.WatchConnected {
						log.Logger(ctx).Info(status.EndpointInfo.URI + " is now connected")
						epConnected = true
					} else {
						log.Logger(ctx).Info(status.EndpointInfo.URI + " is currently disconnected")
					}
					state := s.stateStore.UpdateConnection(epConnected, &status.EndpointInfo)
					newConnState := s.stateStore.BothConnected()
					if state.Status == common.SyncStatusIdle && newConnState && newConnState != initialConnState {
						log.Logger(ctx).Info("Both sides are connected, now launching a sync loop")
						s.task.Run(ctx, false, false)
					}
					bus.Pub(state, TopicState)

				}
				break
			}

		}
	}

}

func (s *Syncer) Serve() {

	ctx := s.serviceCtx

	log.Logger(ctx).Info("Starting Sync Service")
	s.task.SetSnapshotFactory(endpoint.NewSnapshotFactory(s.uuid, s.task.Source, s.task.Target))

	if s.patchStore != nil {
		s.patchStore.Load(s.task.Source, s.task.Target)
	}

	done := make(chan bool, 1)
	go s.dispatch(ctx, done)

	s.task.Start(ctx, s.watches)

	for {
		select {
		case <-s.stop:
			done <- true
			return
		}
	}

}

func (s *Syncer) Stop() {
	s.stop <- true
}

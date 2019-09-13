package peertracker

import (
	"fmt"
	"sync"

	pq "github.com/ipfs/go-ipfs-pq"
	"github.com/ipfs/go-peertaskqueue/peertask"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

// PeerTracker tracks task blocks for a single peer, as well as active tasks
// for that peer
type PeerTracker struct {
	target peer.ID
	// Active is the number of track tasks this peer is currently
	// processing
	// active must be locked around as it will be updated externally
	activelk    sync.Mutex
	active      int
	// activeTasks map[peertask.Identifier]struct{}
	activeTasks map[string]struct{}

	// total number of task tasks for this task
	numTasks int

	// for the PQ interface
	index int

	freezeVal int

	// taskMap map[peertask.Identifier]*peertask.TaskBlock
	taskMap map[string]*peertask.TaskBlock

	// priority queue of tasks belonging to this peer
	taskBlockQueue pq.PQ
}

// New creates a new PeerTracker
func New(target peer.ID) *PeerTracker {
	return &PeerTracker{
		target:         target,
		taskBlockQueue: pq.New(peertask.WrapCompare(peertask.PriorityCompare)),
		// taskMap:        make(map[peertask.Identifier]*peertask.TaskBlock),
		// activeTasks:    make(map[peertask.Identifier]struct{}),
		taskMap:        make(map[string]*peertask.TaskBlock),
		activeTasks:    make(map[string]struct{}),
	}
}

// PeerCompare implements pq.ElemComparator
// returns true if peer 'a' has higher priority than peer 'b'
func PeerCompare(a, b pq.Elem) bool {
	pa := a.(*PeerTracker)
	pb := b.(*PeerTracker)

	// having no tasks means lowest priority
	// having both of these checks ensures stability of the sort
	if pa.numTasks == 0 {
		return false
	}
	if pb.numTasks == 0 {
		return true
	}

	if pa.freezeVal > pb.freezeVal {
		return false
	}
	if pa.freezeVal < pb.freezeVal {
		return true
	}

	if pa.active == pb.active {
		// sorting by taskQueue.Len() aids in cleaning out trash tasks faster
		// if we sorted instead by requests, one peer could potentially build up
		// a huge number of cancelled tasks in the queue resulting in a memory leak
		return pa.taskBlockQueue.Len() > pb.taskBlockQueue.Len()
	}
	return pa.active < pb.active
}

// StartTask signals that a task was started for this peer.
func (p *PeerTracker) StartTask(identifier peertask.Identifier, isBlock bool) {
	p.activelk.Lock()
	// p.activeTasks[identifier] = struct{}{}
	p.activeTasks[p.getTaskId(identifier, isBlock)] = struct{}{}
	p.active++
	p.activelk.Unlock()
}

// TaskDone signals that a task was completed for this peer.
func (p *PeerTracker) TaskDone(identifier peertask.Identifier, isBlock bool) {
	// fmt.Printf("TaskDone() %s-%t\n", identifier, isBlock)
	p.activelk.Lock()
	// delete(p.activeTasks, identifier)
	delete(p.activeTasks, p.getTaskId(identifier, isBlock))
	p.active--
	if p.active < 0 {
		panic("more tasks finished than started!")
	}
	p.activelk.Unlock()
}

// Target returns the peer that this peer tracker tracks tasks for
func (p *PeerTracker) Target() peer.ID {
	return p.target
}

// IsIdle returns true if the peer has no active tasks or queued tasks
func (p *PeerTracker) IsIdle() bool {
	p.activelk.Lock()
	defer p.activelk.Unlock()
	return p.numTasks == 0 && p.active == 0
}

// Index implements pq.Elem.
func (p *PeerTracker) Index() int {
	return p.index
}

// SetIndex implements pq.Elem.
func (p *PeerTracker) SetIndex(i int) {
	p.index = i
}

// PushBlock adds a new block of tasks on to a peers queue from the given
// peer ID, list of tasks, and task block completion function
func (p *PeerTracker) PushBlock(target peer.ID, tasks []peertask.Task, done func(e []peertask.Task)) {

	p.activelk.Lock()
	defer p.activelk.Unlock()

	var priority int
	newTasks := make([]peertask.Task, 0, len(tasks))
	// fmt.Printf("  PushBlock() %d tasks\n", len(tasks))
	for _, task := range tasks {
		taskId := p.getTaskId(task.Identifier, task.IsBlock)

		// If the task is currently active (being processed)
		// if _, ok := p.activeTasks[taskId]; ok {
		if isActiveTaskBlock, ok := p.isAnyTaskWithIdentifierActive(task.Identifier); ok {
			// fmt.Printf("    active task %s\n", task.Identifier)
			// We can only replace tasks that are not active.
			// If the active task could not have been replaced (even if it
			// wasn't active) by the new task, that means the new task is
			// not doing anything useful, so skip adding the new task.
			if isActiveTaskBlock || !task.IsBlock {
				continue
			}
		} else {
			// fmt.Printf("    in-active task %s\n", task.Identifier)
			// If there is already a task with this Identifier
			// if taskBlock, ok := p.taskMap[task.Identifier]; ok {
			if oldTaskId, ok := p.anyQueuedTaskWithIdentifier(task.Identifier); ok {
				// fmt.Printf("      (existing task %s)\n", task.Identifier)
				taskBlock, ok := p.taskMap[oldTaskId]
				if !ok {
					panic("anyQueuedTaskWithIdentifier() returned non-existent task id")
				}

				// If the new task has a higher priority than the old task block,
				// move the old task block towards the front of the queue.
				if task.Priority > taskBlock.Priority {
					taskBlock.Priority = task.Priority
					p.taskBlockQueue.Update(taskBlock.Index())
				}				

				// TODO: Replacing the task may result in the block exceeding the message size limit
				// Replace the task (if it's replaceable)
				if taskBlock.ReplaceTask(task) {
					// Update the mapping from task -> task block
					if taskId != oldTaskId {
						p.taskMap[taskId] = taskBlock
						delete(p.taskMap, oldTaskId)
					}
				}

				// If a task with the Identifier exists, we don't need to add
				// the new task to the queue
				continue
			}
		}

		// The block priority will be set to the highest priority of all the
		// tasks in the block
		if task.Priority > priority {
			priority = task.Priority
		}

		newTasks = append(newTasks, task)
	}

	if len(newTasks) == 0 {
		return
	}

	// Add the task block to the queue, and create a mapping of
	// task -> task block
	taskBlock := peertask.NewTaskBlock(newTasks, priority, target, done)
	p.taskBlockQueue.Push(taskBlock)
	for _, task := range newTasks {
		// p.taskMap[task.Identifier] = taskBlock
		p.taskMap[p.getTaskId(task.Identifier, task.IsBlock)] = taskBlock
	}
	p.numTasks += len(newTasks)
}

// PopBlock removes a block of tasks from this peers queue
func (p *PeerTracker) PopBlock() *peertask.TaskBlock {
	// fmt.Printf("PopBlock()\n")
	var out *peertask.TaskBlock
	for p.taskBlockQueue.Len() > 0 && p.freezeVal == 0 {
		out = p.taskBlockQueue.Pop().(*peertask.TaskBlock)

		for _, task := range out.Tasks {
			// delete(p.taskMap, task.Identifier)
			delete(p.taskMap, p.getTaskId(task.Identifier, task.IsBlock))
		}
		out.PruneTasks()

		// fmt.Printf("  %d tasks\n", len(out.Tasks))
		if len(out.Tasks) > 0 {
			for _, task := range out.Tasks {
				// fmt.Printf("  StartTask() %s\n", task)
				p.numTasks--
				// p.StartTask(task.Identifier)
				p.StartTask(task.Identifier, task.IsBlock)
			}
		} else {
			out = nil
			continue
		}
		break
	}
	return out
}

// Remove removes the task with the given identifier from this peers queue
func (p *PeerTracker) Remove(identifier peertask.Identifier) bool {
	// taskBlock, ok := p.taskMap[identifier]
	// if ok {
	// 	// taskBlock.MarkPrunable(identifier)
	// 	taskBlock.MarkPrunable(taskId)
	// 	p.numTasks--
	// }
	// fmt.Printf("Remove() %s\n", identifier)

	taskIdBlock := p.getTaskId(identifier, true)
	// fmt.Printf("  remove %s\n", taskIdBlock)
	taskBlockIsBlock, ok := p.taskMap[taskIdBlock]
	if ok {
		taskBlockIsBlock.MarkPrunable(taskIdBlock, true)
		p.numTasks--
	}

	taskIdNotBlock := p.getTaskId(identifier, false)
	// fmt.Printf("  remove %s\n", taskIdNotBlock)
	taskBlockNotBlock, oknb := p.taskMap[taskIdNotBlock]
	if oknb {
		taskBlockNotBlock.MarkPrunable(taskIdNotBlock, false)
		p.numTasks--
	}
	return ok || oknb
}

// Freeze increments the freeze value for this peer. While a peer is frozen
// (freeze value > 0) it will not execute tasks.
func (p *PeerTracker) Freeze() {
	p.freezeVal++
}

// Thaw decrements the freeze value for this peer. While a peer is frozen
// (freeze value > 0) it will not execute tasks.
func (p *PeerTracker) Thaw() bool {
	p.freezeVal -= (p.freezeVal + 1) / 2
	return p.freezeVal <= 0
}

// FullThaw completely unfreezes this peer so it can execute tasks.
func (p *PeerTracker) FullThaw() {
	p.freezeVal = 0
}

// IsFrozen returns whether this peer is frozen and unable to execute tasks.
func (p *PeerTracker) IsFrozen() bool {
	return p.freezeVal > 0
}

func (p *PeerTracker) getTaskId(identifier peertask.Identifier, isBlock bool) string {
	return fmt.Sprintf("%s-%t", identifier, isBlock)
}

func (p *PeerTracker) isAnyTaskWithIdentifierActive(identifier peertask.Identifier) (bool, bool) {
	taskIdBlock := p.getTaskId(identifier, true)
	if _, ok := p.activeTasks[taskIdBlock]; ok {
		return true, true
	}

	taskIdNotBlock := p.getTaskId(identifier, false)
	if _, ok := p.activeTasks[taskIdNotBlock]; ok {
		return false, true
	}
	return false, false
}

func (p *PeerTracker) anyQueuedTaskWithIdentifier(identifier peertask.Identifier) (string, bool) {
	taskIdBlock := p.getTaskId(identifier, true)
	if _, ok := p.taskMap[taskIdBlock]; ok {
		return taskIdBlock, true
	}

	taskIdNotBlock := p.getTaskId(identifier, false)
	if _, ok := p.taskMap[taskIdNotBlock]; ok {
		return taskIdNotBlock, true
	}
	return "", false
}

-- Class (b): Are-We-Fast-Yet Richards OS Scheduler (idiomatic Lua OOP with __index chains)
local ID_IDLE = 1
local ID_WORKER = 2
local ID_HANDLER_A = 3
local ID_HANDLER_B = 4
local ID_DEVICE_A = 5
local ID_DEVICE_B = 6
local NUMBER_OF_IDS = 6

local KIND_DEVICE = 0
local KIND_WORK = 1

local DATA_SIZE = 4

local function class(base)
  local c = {}
  c.__index = c
  if base then
    setmetatable(c, { __index = base })
  end
  return c
end

-- Packet
local Packet = class()
function Packet.new(link, identity, kind)
  local data = { 0, 0, 0, 0 }
  return setmetatable({
    link = link,
    identity = identity,
    kind = kind,
    datum = 1,
    data = data,
  }, Packet)
end

function Packet:appendTo(queueHead)
  self.link = nil
  if not queueHead then return self end
  local mouse = queueHead
  local link = mouse.link
  while link do
    mouse = link
    link = mouse.link
  end
  mouse.link = self
  return queueHead
end

-- TaskState
local TaskState = class()
function TaskState.new()
  return setmetatable({
    packetPending = false,
    taskWaiting = false,
    taskHolding = false,
  }, TaskState)
end

function TaskState:setRunning()
  self.packetPending = false
  self.taskWaiting = false
  self.taskHolding = false
end

function TaskState:setWaiting()
  self.packetPending = false
  self.taskWaiting = true
  self.taskHolding = false
end

function TaskState:setWaitingWithPacket()
  self.packetPending = true
  self.taskWaiting = true
  self.taskHolding = false
end

function TaskState:isWaitingWithPacket()
  return self.packetPending and self.taskWaiting and (not self.taskHolding)
end

function TaskState:isTaskHoldingOrWaiting()
  return self.taskHolding or ((not self.packetPending) and self.taskWaiting)
end

-- TaskControlBlock (inherits TaskState via 2-hop __index chain!)
local TaskControlBlock = class(TaskState)
function TaskControlBlock.new(link, identity, priority, initialWorkQueue, initialState, fn, handle)
  local self = setmetatable({
    link = link,
    identity = identity,
    priority = priority,
    input = initialWorkQueue,
    packetPending = initialState.packetPending,
    taskWaiting = initialState.taskWaiting,
    taskHolding = initialState.taskHolding,
    fn = fn,
    handle = handle,
  }, TaskControlBlock)
  return self
end

function TaskControlBlock:addInputAndCheckPriority(packet, oldTask)
  if not self.input then
    self.input = packet
    self.packetPending = true
    if self.priority > oldTask.priority then
      return self
    end
  else
    self.input = packet:appendTo(self.input)
  end
  return oldTask
end

function TaskControlBlock:runTask()
  local msg
  if self:isWaitingWithPacket() then
    msg = self.input
    self.input = msg.link
    if not self.input then
      self:setRunning()
    else
      self.packetPending = true
      self.taskWaiting = false
      self.taskHolding = false
    end
  else
    msg = nil
  end
  return self.fn(self.handle, msg)
end

-- Scheduler
local Scheduler = class()
function Scheduler.new()
  return setmetatable({
    taskList = nil,
    currentTask = nil,
    currentTaskIdentity = 0,
    taskTable = {},
    queuePacketCount = 0,
    holdCount = 0,
  }, Scheduler)
end

function Scheduler:addTask(identity, priority, work, state, fn, data)
  local t = TaskControlBlock.new(self.taskList, identity, priority, work, state, fn, data)
  self.taskList = t
  self.taskTable[identity] = t
end

function Scheduler:findTask(identity)
  local t = self.taskTable[identity]
  assert(t ~= nil, "findTask failed")
  return t
end

function Scheduler:holdSelf()
  self.holdCount = self.holdCount + 1
  self.currentTask.taskHolding = true
  return self.currentTask.link
end

function Scheduler:release(identity)
  local t = self:findTask(identity)
  t.taskHolding = false
  if t.priority > self.currentTask.priority then
    return t
  end
  return self.currentTask
end

function Scheduler:queuePacket(packet)
  local t = self:findTask(packet.identity)
  self.queuePacketCount = self.queuePacketCount + 1
  packet.link = nil
  packet.identity = self.currentTaskIdentity
  return t:addInputAndCheckPriority(packet, self.currentTask)
end

function Scheduler:markWaiting()
  self.currentTask.taskWaiting = true
  return self.currentTask
end

function Scheduler:schedule()
  self.currentTask = self.taskList
  while self.currentTask do
    if self.currentTask:isTaskHoldingOrWaiting() then
      self.currentTask = self.currentTask.link
    else
      self.currentTaskIdentity = self.currentTask.identity
      self.currentTask = self.currentTask:runTask()
    end
  end
end

-- Task records
local IdleTask = class()
function IdleTask.new(scheduler, control, count)
  return setmetatable({ scheduler = scheduler, control = control, count = count }, IdleTask)
end
function IdleTask:run(work)
  self.count = self.count - 1
  if self.count == 0 then
    return self.scheduler:holdSelf()
  end
  if (self.control & 1) == 0 then
    self.control = self.control >> 1
    return self.scheduler:release(ID_DEVICE_A)
  else
    self.control = (self.control >> 1) ~ 53256
    return self.scheduler:release(ID_DEVICE_B)
  end
end

local WorkerTask = class()
function WorkerTask.new(scheduler, dest, count)
  return setmetatable({ scheduler = scheduler, destination = dest, count = count }, WorkerTask)
end
function WorkerTask:run(work)
  if not work then
    return self.scheduler:markWaiting()
  end
  if self.destination == ID_HANDLER_A then
    self.destination = ID_HANDLER_B
  else
    self.destination = ID_HANDLER_A
  end
  work.identity = self.destination
  work.datum = 1
  for i = 1, DATA_SIZE do
    self.count = self.count + 1
    if self.count > 26 then self.count = 1 end
    work.data[i] = 65 + self.count - 1
  end
  return self.scheduler:queuePacket(work)
end

local HandlerTask = class()
function HandlerTask.new(scheduler)
  return setmetatable({ scheduler = scheduler, workIn = nil, deviceIn = nil }, HandlerTask)
end
function HandlerTask:run(work)
  if work then
    if work.kind == KIND_WORK then
      self.workIn = work:appendTo(self.workIn)
    else
      self.deviceIn = work:appendTo(self.deviceIn)
    end
  end
  if self.workIn then
    local workPacket = self.workIn
    local count = workPacket.datum
    if count > DATA_SIZE then
      self.workIn = workPacket.link
      return self.scheduler:queuePacket(workPacket)
    end
    if self.deviceIn then
      local devPacket = self.deviceIn
      self.deviceIn = devPacket.link
      devPacket.datum = workPacket.data[count]
      workPacket.datum = count + 1
      return self.scheduler:queuePacket(devPacket)
    end
  end
  return self.scheduler:markWaiting()
end

local DeviceTask = class()
function DeviceTask.new(scheduler)
  return setmetatable({ scheduler = scheduler, pending = nil }, DeviceTask)
end
function DeviceTask:run(work)
  local fnWork = work
  if not fnWork then
    if not self.pending then
      return self.scheduler:markWaiting()
    end
    fnWork = self.pending
    self.pending = nil
    return self.scheduler:queuePacket(fnWork)
  end
  self.pending = fnWork
  return self.scheduler:holdSelf()
end

local function runOnce()
  local s = Scheduler.new()
  local stRun = TaskState.new(); stRun:setRunning()
  local stWait = TaskState.new(); stWait:setWaiting()
  local stWaitPkt = TaskState.new(); stWaitPkt:setWaitingWithPacket()

  s:addTask(ID_IDLE, 0, nil, stRun, IdleTask.run, IdleTask.new(s, 1, 10000))

  local wkq = Packet.new(nil, 0, KIND_WORK)
  wkq = Packet.new(wkq, 0, KIND_WORK)
  s:addTask(ID_WORKER, 1000, wkq, stWaitPkt, WorkerTask.run, WorkerTask.new(s, ID_HANDLER_A, 0))

  wkq = Packet.new(nil, ID_DEVICE_A, KIND_DEVICE)
  wkq = Packet.new(wkq, ID_DEVICE_A, KIND_DEVICE)
  wkq = Packet.new(wkq, ID_DEVICE_A, KIND_DEVICE)
  s:addTask(ID_HANDLER_A, 2000, wkq, stWaitPkt, HandlerTask.run, HandlerTask.new(s))

  wkq = Packet.new(nil, ID_DEVICE_B, KIND_DEVICE)
  wkq = Packet.new(wkq, ID_DEVICE_B, KIND_DEVICE)
  wkq = Packet.new(wkq, ID_DEVICE_B, KIND_DEVICE)
  s:addTask(ID_HANDLER_B, 3000, wkq, stWaitPkt, HandlerTask.run, HandlerTask.new(s))

  s:addTask(ID_DEVICE_A, 4000, nil, stWait, DeviceTask.run, DeviceTask.new(s))
  s:addTask(ID_DEVICE_B, 5000, nil, stWait, DeviceTask.run, DeviceTask.new(s))

  s:schedule()
  assert(s.queuePacketCount == 23246 and s.holdCount == 9297)
  return s.queuePacketCount, s.holdCount
end

local qTotal, hTotal = 0, 0
for _ = 1, 20 do
  local q, h = runOnce()
  qTotal = qTotal + q
  hTotal = hTotal + h
end
print("richards", qTotal, hTotal)

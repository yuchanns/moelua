-- Class (b): Are-We-Fast-Yet DeltaBlue Constraint Solver (deep Lua OOP __index hierarchy)
local function class(base)
  local c = {}
  c.__index = c
  if base then
    setmetatable(c, { __index = base })
  end
  return c
end

-- Strength
local Strength = class()
function Strength.new(val, name)
  return setmetatable({ arithmeticValue = val, name = name }, Strength)
end
function Strength.stronger(s1, s2) return s1.arithmeticValue < s2.arithmeticValue end
function Strength.weaker(s1, s2) return s1.arithmeticValue > s2.arithmeticValue end
function Strength.weakest(s1, s2)
  if s1.arithmeticValue > s2.arithmeticValue then return s1 end
  return s2
end

local REQUIRED = Strength.new(0, "required")
local STRONG_PREFERRED = Strength.new(1, "strongPreferred")
local PREFERRED = Strength.new(2, "preferred")
local STRONG_DEFAULT = Strength.new(3, "strongDefault")
local NORMAL = Strength.new(4, "normal")
local WEAK_DEFAULT = Strength.new(5, "weakDefault")
local WEAKEST = Strength.new(6, "weakest")

local DIR_NONE = 0
local DIR_FORWARD = 1
local DIR_BACKWARD = 2

-- Vector helper
local Vector = class()
function Vector.new()
  return setmetatable({ items = {}, n = 0 }, Vector)
end
function Vector:append(x)
  local n = self.n + 1
  self.n = n
  self.items[n] = x
end
function Vector:remove(x)
  local items = self.items
  local n = self.n
  for i = 1, n do
    if items[i] == x then
      items[i] = items[n]
      items[n] = nil
      self.n = n - 1
      return
    end
  end
end
function Vector:size() return self.n end
function Vector:at(i) return self.items[i] end

-- Variable
local Variable = class()
function Variable.new(name, initialValue)
  return setmetatable({
    name = name,
    value = initialValue or 0,
    constraints = Vector.new(),
    determinedBy = nil,
    mark = 0,
    walkStrength = WEAKEST,
    stay = true,
  }, Variable)
end
function Variable:addConstraint(c) self.constraints:append(c) end
function Variable:removeConstraint(c)
  self.constraints:remove(c)
  if self.determinedBy == c then self.determinedBy = nil end
end

-- Abstract Constraint
local Constraint = class()
function Constraint:isInput() return false end
function Constraint:addConstraint(planner)
  self:addToGraph()
  planner:incrementalAdd(self)
end
function Constraint:destroyConstraint(planner)
  if self:isSatisfied() then
    planner:incrementalRemove(self)
  end
  self:removeFromGraph()
end
function Constraint:satisfy(mark, planner)
  self:chooseMethod(mark)
  if not self:isSatisfied() then
    if self.strength == REQUIRED then
      error("Could not satisfy a required constraint")
    end
    return nil
  end
  self:markInputs(mark)
  local out = self:output()
  local overridden = out.determinedBy
  if overridden then overridden:markUnsatisfied() end
  out.determinedBy = self
  if not planner:addPropagate(self, mark) then
    error("Cycle encountered")
  end
  out.mark = mark
  return overridden
end

-- UnaryConstraint
local UnaryConstraint = class(Constraint)
function UnaryConstraint.init(self, v, strength, planner)
  self.strength = strength
  self.myOutput = v
  self.satisfied = false
  self:addConstraint(planner)
  return self
end
function UnaryConstraint:addToGraph()
  self.myOutput:addConstraint(self)
  self.satisfied = false
end
function UnaryConstraint:removeFromGraph()
  if self.myOutput then self.myOutput:removeConstraint(self) end
  self.satisfied = false
end
function UnaryConstraint:chooseMethod(mark)
  self.satisfied = (self.myOutput.mark ~= mark) and Strength.stronger(self.strength, self.myOutput.walkStrength)
end
function UnaryConstraint:isSatisfied() return self.satisfied end
function UnaryConstraint:markInputs(mark) end
function UnaryConstraint:output() return self.myOutput end
function UnaryConstraint:recalculate()
  self.myOutput.walkStrength = self.strength
  self.myOutput.stay = not self:isInput()
  if self.myOutput.stay then self:execute() end
end
function UnaryConstraint:markUnsatisfied() self.satisfied = false end
function UnaryConstraint:inputsKnown(mark) return true end

local StayConstraint = class(UnaryConstraint)
function StayConstraint.new(v, strength, planner)
  return UnaryConstraint.init(setmetatable({}, StayConstraint), v, strength, planner)
end
function StayConstraint:execute() end

local EditConstraint = class(UnaryConstraint)
function EditConstraint.new(v, strength, planner)
  return UnaryConstraint.init(setmetatable({}, EditConstraint), v, strength, planner)
end
function EditConstraint:isInput() return true end
function EditConstraint:execute() end

-- BinaryConstraint
local BinaryConstraint = class(Constraint)
function BinaryConstraint.init(self, v1, v2, strength, planner)
  self.strength = strength
  self.v1 = v1
  self.v2 = v2
  self.direction = DIR_NONE
  self:addConstraint(planner)
  return self
end
function BinaryConstraint:chooseMethod(mark)
  if self.v1.mark == mark then
    if self.v2.mark ~= mark and Strength.stronger(self.strength, self.v2.walkStrength) then
      self.direction = DIR_FORWARD
    else
      self.direction = DIR_NONE
    end
  elseif self.v2.mark == mark then
    if self.v1.mark ~= mark and Strength.stronger(self.strength, self.v1.walkStrength) then
      self.direction = DIR_BACKWARD
    else
      self.direction = DIR_NONE
    end
  elseif Strength.weaker(self.v1.walkStrength, self.v2.walkStrength) then
    if Strength.stronger(self.strength, self.v1.walkStrength) then
      self.direction = DIR_BACKWARD
    else
      self.direction = DIR_NONE
    end
  else
    if Strength.stronger(self.strength, self.v2.walkStrength) then
      self.direction = DIR_FORWARD
    else
      self.direction = DIR_NONE
    end
  end
end
function BinaryConstraint:addToGraph()
  self.v1:addConstraint(self)
  self.v2:addConstraint(self)
  self.direction = DIR_NONE
end
function BinaryConstraint:removeFromGraph()
  if self.v1 then self.v1:removeConstraint(self) end
  if self.v2 then self.v2:removeConstraint(self) end
  self.direction = DIR_NONE
end
function BinaryConstraint:isSatisfied() return self.direction ~= DIR_NONE end
function BinaryConstraint:markInputs(mark) self:input().mark = mark end
function BinaryConstraint:input()
  if self.direction == DIR_FORWARD then return self.v1 else return self.v2 end
end
function BinaryConstraint:output()
  if self.direction == DIR_FORWARD then return self.v2 else return self.v1 end
end
function BinaryConstraint:recalculate()
  local ihn = self:input()
  local out = self:output()
  out.walkStrength = Strength.weakest(self.strength, ihn.walkStrength)
  out.stay = ihn.stay
  if out.stay then self:execute() end
end
function BinaryConstraint:markUnsatisfied() self.direction = DIR_NONE end
function BinaryConstraint:inputsKnown(mark)
  local i = self:input()
  return i.mark == mark or i.stay or i.determinedBy == nil
end

local EqualityConstraint = class(BinaryConstraint)
function EqualityConstraint.new(v1, v2, strength, planner)
  return BinaryConstraint.init(setmetatable({}, EqualityConstraint), v1, v2, strength, planner)
end
function EqualityConstraint:execute()
  self:output().value = self:input().value
end

local ScaleConstraint = class(BinaryConstraint)
function ScaleConstraint.new(src, scale, offset, dest, strength, planner)
  local self = setmetatable({ scale = scale, offset = offset }, ScaleConstraint)
  return BinaryConstraint.init(self, src, dest, strength, planner)
end
function ScaleConstraint:addToGraph()
  BinaryConstraint.addToGraph(self)
  self.scale:addConstraint(self)
  self.offset:addConstraint(self)
end
function ScaleConstraint:removeFromGraph()
  BinaryConstraint.removeFromGraph(self)
  if self.scale then self.scale:removeConstraint(self) end
  if self.offset then self.offset:removeConstraint(self) end
end
function ScaleConstraint:markInputs(mark)
  BinaryConstraint.markInputs(self, mark)
  self.scale.mark = mark
  self.offset.mark = mark
end
function ScaleConstraint:execute()
  if self.direction == DIR_FORWARD then
    self.v2.value = self.v1.value * self.scale.value + self.offset.value
  else
    self.v1.value = (self.v2.value - self.offset.value) // self.scale.value
  end
end
function ScaleConstraint:recalculate()
  local ihn = self:input()
  local out = self:output()
  out.walkStrength = Strength.weakest(self.strength, ihn.walkStrength)
  out.stay = ihn.stay and self.scale.stay and self.offset.stay
  if out.stay then self:execute() end
end

-- Plan
local Plan = class()
function Plan.new()
  return setmetatable({ v = Vector.new() }, Plan)
end
function Plan:addConstraint(c) self.v:append(c) end
function Plan:execute()
  local items = self.v.items
  for i = 1, self.v.n do
    items[i]:execute()
  end
end

-- Planner
local Planner = class()
function Planner.new()
  return setmetatable({ currentMark = 0 }, Planner)
end
function Planner:newMark()
  self.currentMark = self.currentMark + 1
  return self.currentMark
end
function Planner:incrementalAdd(c)
  local mark = self:newMark()
  local overridden = c:satisfy(mark, self)
  while overridden do
    overridden = overridden:satisfy(mark, self)
  end
end
function Planner:incrementalRemove(c)
  local out = c:output()
  c:markUnsatisfied()
  c:removeFromGraph()
  local unsatisfied = self:removePropagateFrom(out)
  local strength = REQUIRED
  repeat
    local items = unsatisfied.items
    for i = 1, unsatisfied.n do
      local u = items[i]
      if u.strength == strength then
        self:incrementalAdd(u)
      end
    end
    if strength == REQUIRED then strength = STRONG_PREFERRED
    elseif strength == STRONG_PREFERRED then strength = PREFERRED
    elseif strength == PREFERRED then strength = STRONG_DEFAULT
    elseif strength == STRONG_DEFAULT then strength = NORMAL
    elseif strength == NORMAL then strength = WEAK_DEFAULT
    elseif strength == WEAK_DEFAULT then strength = WEAKEST
    else strength = nil end
  until strength == nil
end
function Planner:addConstraintsConsumingTo(v, coll)
  local determining = v.determinedBy
  local items = v.constraints.items
  for i = 1, v.constraints.n do
    local c = items[i]
    if c ~= determining and c:isSatisfied() then
      coll:append(c)
    end
  end
end
function Planner:addPropagate(c, mark)
  local todo = Vector.new()
  todo:append(c)
  while todo.n > 0 do
    local d = todo.items[todo.n]
    todo.items[todo.n] = nil
    todo.n = todo.n - 1
    if d:output().mark == mark then
      self:incrementalRemove(c)
      return false
    end
    d:recalculate()
    self:addConstraintsConsumingTo(d:output(), todo)
  end
  return true
end
function Planner:removePropagateFrom(out)
  out.determinedBy = nil
  out.walkStrength = WEAKEST
  out.stay = true
  local unsatisfied = Vector.new()
  local todo = Vector.new()
  todo:append(out)
  while todo.n > 0 do
    local v = todo.items[todo.n]
    todo.items[todo.n] = nil
    todo.n = todo.n - 1
    local items = v.constraints.items
    for i = 1, v.constraints.n do
      local c = items[i]
      if not c:isSatisfied() then
        unsatisfied:append(c)
      end
    end
    local determining = v.determinedBy
    for i = 1, v.constraints.n do
      local nextC = items[i]
      if nextC ~= determining and nextC:isSatisfied() then
        nextC:recalculate()
        todo:append(nextC:output())
      end
    end
  end
  return unsatisfied
end
function Planner:makePlan(sources)
  local mark = self:newMark()
  local plan = Plan.new()
  local todo = sources
  while todo.n > 0 do
    local c = todo.items[todo.n]
    todo.items[todo.n] = nil
    todo.n = todo.n - 1
    if c:output().mark ~= mark and c:inputsKnown(mark) then
      plan:addConstraint(c)
      c:output().mark = mark
      self:addConstraintsConsumingTo(c:output(), todo)
    end
  end
  return plan
end
function Planner:extractPlanFromConstraints(constraints)
  local sources = Vector.new()
  local items = constraints.items
  for i = 1, constraints.n do
    local c = items[i]
    if c:isInput() and c:isSatisfied() then
      sources:append(c)
    end
  end
  return self:makePlan(sources)
end
function Planner:change(v, newValue)
  local edit = EditConstraint.new(v, PREFERRED, self)
  local edits = Vector.new()
  edits:append(edit)
  local plan = self:extractPlanFromConstraints(edits)
  for _ = 1, 10 do
    v.value = newValue
    plan:execute()
  end
  edit:destroyConstraint(self)
end

local function chainTest(n)
  local planner = Planner.new()
  local prev, first, last = nil, nil, nil
  for i = 0, n do
    local v = Variable.new("v", 0)
    if prev then EqualityConstraint.new(prev, v, REQUIRED, planner) end
    if i == 0 then first = v end
    if i == n then last = v end
    prev = v
  end
  StayConstraint.new(last, STRONG_DEFAULT, planner)
  local edit = EditConstraint.new(first, PREFERRED, planner)
  local edits = Vector.new()
  edits:append(edit)
  local plan = planner:extractPlanFromConstraints(edits)
  for i = 0, 99 do
    first.value = i
    plan:execute()
    assert(last.value == i)
  end
  return last.value
end

local function projectionTest(n)
  local planner = Planner.new()
  local scale = Variable.new("scale", 10)
  local offset = Variable.new("offset", 1000)
  local src, dst = nil, nil
  local dests = Vector.new()
  for i = 0, n - 1 do
    src = Variable.new("src", i)
    dst = Variable.new("dst", i)
    dests:append(dst)
    StayConstraint.new(src, NORMAL, planner)
    ScaleConstraint.new(src, scale, offset, dst, REQUIRED, planner)
  end
  planner:change(src, 17)
  assert(dst.value == 1170)
  planner:change(dst, 1050)
  assert(src.value == 5)
  planner:change(scale, 5)
  for i = 0, n - 2 do assert(dests:at(i + 1).value == i * 5 + 1000) end
  planner:change(offset, 2000)
  for i = 0, n - 2 do assert(dests:at(i + 1).value == i * 5 + 2000) end
  return dst.value
end

local acc = 0
for _ = 1, 25 do
  acc = acc + chainTest(100) + projectionTest(100)
end
print("deltablue", acc)

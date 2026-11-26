-- Class (b): Realistic Lua Game/UI ECS Tick Loop
-- Combines __index OOP method dispatch, ipairs/pairs iteration, math.floor/abs/sqrt, and field mutations.
local Entity = {}
Entity.__index = Entity

function Entity.new(id, x, y, vx, vy)
  return setmetatable({
    id = id, x = x, y = y, vx = vx, vy = vy,
    hp = 100, score = 0, alive = true,
  }, Entity)
end

function Entity:step(dt)
  self.x = self.x + self.vx * dt
  self.y = self.y + self.vy * dt
  if self.x > 100.0 or self.x < -100.0 then self.vx = -self.vx end
  if self.y > 100.0 or self.y < -100.0 then self.vy = -self.vy end
  local d = math.sqrt(self.x * self.x + self.y * self.y)
  self.score = self.score + math.floor(d) + math.floor(math.abs(self.vx))
end

local N = 2000
local entities = table.create(N)
for i = 1, N do
  entities[i] = Entity.new(i, (i % 50) * 1.5, (i % 37) * 1.2, ((i % 7) - 3) * 0.5, ((i % 5) - 2) * 0.7)
end

for _ = 1, 200 do
  for _, e in ipairs(entities) do
    e:step(0.016)
  end
end

local sum = 0
for _, e in ipairs(entities) do
  sum = sum + e.score
end
print("ecs_stdlib", sum)

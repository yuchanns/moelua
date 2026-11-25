-- Class (a): Tight integer for-loop (50M iterations)
local function run(n)
  local s = 0
  for i = 1, n do
    s = s + (i & 7) - (i & 3)
  end
  return s
end
local r = run(50000000)
print("forloop_int", r)

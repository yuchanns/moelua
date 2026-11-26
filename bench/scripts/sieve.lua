-- Class (a): Are-We-Fast-Yet Sieve of Eratosthenes
local function sieve(flags, size)
  local prime_count = 0
  for i = 2, size do
    if flags[i] then
      prime_count = prime_count + 1
      local k = i + i
      while k <= size do
        flags[k] = false
        k = k + i
      end
    end
  end
  return prime_count
end

local last = 0
for _ = 1, 500 do
  local flags = table.create(5000, 0)
  for i = 1, 5000 do flags[i] = true end
  last = sieve(flags, 5000)
end
assert(last == 669)
print("sieve", last)

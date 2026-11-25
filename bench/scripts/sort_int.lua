-- Class (c): Dense integer array generation + table.sort (50,000 ints x 5 rounds)
local N = 50000
local checksum = 0
local seed = 123456789
for _ = 1, 5 do
  local a = table.create(N)
  for i = 1, N do
    seed = (seed * 1664525 + 1013904223) & 0xffffffff
    a[i] = seed
  end
  table.sort(a)
  checksum = checksum + a[1] + a[N // 2] + a[N]
end
print("sort_int", checksum)

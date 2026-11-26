-- Class (c): Dense 160x160 float matrix multiply (flat 1D float arrays)
local N = 160
local A = table.create(N * N)
local B = table.create(N * N)
local C = table.create(N * N)
for i = 1, N * N do
  A[i] = ((i * 17) % 97) * 0.01
  B[i] = ((i * 31) % 89) * 0.01
  C[i] = 0.0
end
for i = 0, N - 1 do
  local rowA = i * N
  for k = 0, N - 1 do
    local aik = A[rowA + k + 1]
    local rowB = k * N
    for j = 1, N do
      C[rowA + j] = C[rowA + j] + aik * B[rowB + j]
    end
  end
end
local sum = 0.0
for i = 1, N * N do sum = sum + C[i] end
print("matmul", string.format("%.6f", sum))

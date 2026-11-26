-- Class (a): Recursive Fibonacci
local function fib(n)
  if n < 2 then return n end
  return fib(n - 1) + fib(n - 2)
end
local r = fib(34)
assert(r == 5702887)
print("fib", r)

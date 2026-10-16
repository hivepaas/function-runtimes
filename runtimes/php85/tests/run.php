<?php

declare(strict_types=1);

// The PHP side's own tests, without a framework:
//
//   docker run --rm -v "$PWD:/src:ro" -w /src php:8.5-cli-trixie php runtimes/php85/tests/run.php

require __DIR__ . '/../runtime/Hivepaas/Bytes.php';
require __DIR__ . '/../runtime/Hivepaas/Request.php';
require __DIR__ . '/../runtime/Hivepaas/Context.php';
require __DIR__ . '/../runtime/Hivepaas/Runtime/CallError.php';
require __DIR__ . '/../runtime/Hivepaas/Runtime/Response.php';

$failed = 0;
foreach (glob(__DIR__ . '/*_test.php') as $file) {
    require $file;
}
foreach (get_defined_functions()['user'] as $fn) {
    if (!str_starts_with($fn, 'test')) {
        continue;
    }
    try {
        $fn();
        echo "ok   {$fn}\n";
    } catch (\Throwable $e) {
        $failed++;
        echo "FAIL {$fn}: {$e->getMessage()} ({$e->getFile()}:{$e->getLine()})\n";
    }
}
exit($failed > 0 ? 1 : 0);

function same(mixed $want, mixed $got, string $what = ''): void
{
    if ($want !== $got) {
        throw new \RuntimeException(sprintf('%s: got %s, want %s', $what, var_export($got, true), var_export($want, true)));
    }
}

function fails(callable $fn, string $says, string $outcome = 'error'): void
{
    try {
        $fn();
    } catch (\Hivepaas\Runtime\CallError $e) {
        if (!str_contains($e->getMessage(), $says) || $e->outcome !== $outcome) {
            throw new \RuntimeException("failed with {$e->outcome}: {$e->getMessage()}");
        }
        return;
    }
    throw new \RuntimeException("did not fail: {$says}");
}

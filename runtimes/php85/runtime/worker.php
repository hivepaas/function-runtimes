<?php

declare(strict_types=1);

// A PHP process of the runtime: it loads the function's handler, then runs the
// calls the server hands it, one at a time, until the server closes its calls.

require __DIR__ . '/Hivepaas/Bytes.php';
require __DIR__ . '/Hivepaas/Request.php';
require __DIR__ . '/Hivepaas/Context.php';
require __DIR__ . '/Hivepaas/Runtime/CallError.php';
require __DIR__ . '/Hivepaas/Runtime/Response.php';
require __DIR__ . '/Hivepaas/Runtime/Worker.php';

\Hivepaas\Runtime\Worker::begin();
try {
    \Hivepaas\Runtime\Worker::autoload();
    // At the top level, so that what the file declares - its variables too -
    // is global, as when PHP runs the file itself.
    require \Hivepaas\Runtime\Worker::entrypoint();
} catch (\Throwable $e) {
    \Hivepaas\Runtime\Worker::notLoaded($e);
}
\Hivepaas\Runtime\Worker::serve();

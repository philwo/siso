#!/bin/sh
# Copyright 2024 The Chromium Authors
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

# generate hmap test data using chromium's
# build/config/ios/write_framework_hmap.py

set -e

if [ ! -f "${CHROMIUM_SRC_DIR}/build/config/ios/write_framework_hmap.py" ]; then
    echo "build/config/ios/write_framework_hmap.py does not exist " \
         "in \${CHROMIUM_SRC_DIR}" 1>&2
    echo "set CHROMIUM_SRC_DIR and re-run" 1>&2
    exit 1
fi

# expected to run this on MacOSX. /tmp -> /private/tmp
mkdir -p /private/tmp/siso-scandeps-test/out/siso
(cd /private/tmp/siso-scandeps-test/out/siso && \
 python3 "${CHROMIUM_SRC_DIR}/build/config/ios/write_framework_hmap.py" \
    test.hmap \
    Foo.framework \
    ../../ios/fooFramework/Foo.h \
    ../../ios/fooFramework/Bar.h \
    ../../ios/fooFramework/Baz.h)
cp /private/tmp/siso-scandeps-test/out/siso/test.hmap testdata/test.hmap

# expected
#  Foo/Foo.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h
#  Foo/Bar.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h
#  Foo/Baz.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h
#  Foo.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Foo.h
#  Bar.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Bar.h
#  Baz.h -> /private/tmp/siso-scandeps-test/ios/fooFramework/Baz.h

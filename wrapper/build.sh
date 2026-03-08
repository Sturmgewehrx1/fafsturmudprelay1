#!/bin/bash
# Build the Java wrapper JAR
set -e
cd "$(dirname "$0")"
javac com/faforever/iceadapter/IceAdapter.java
jar cfe faf-ice-adapter.jar com.faforever.iceadapter.IceAdapter com/faforever/iceadapter/IceAdapter.class
echo "Built wrapper/faf-ice-adapter.jar"

# Copy to build/client/ if the directory exists or create it
mkdir -p ../build/client
cp faf-ice-adapter.jar ../build/client/faf-ice-adapter.jar
echo "Copied faf-ice-adapter.jar to build/client/"

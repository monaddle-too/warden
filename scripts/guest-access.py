#!/usr/bin/env python3
import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'host'))
from warden.guest import main
try:sys.exit(main())
except (OSError,ValueError,KeyError) as error:
    print('warden guest:',error,file=sys.stderr);sys.exit(1)

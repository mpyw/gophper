<?php
usleep((int) ($_GET['ms'] ?? 300) * 1000);
echo "done\n";
